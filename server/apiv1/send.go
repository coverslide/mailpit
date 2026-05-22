package apiv1

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/smtpd"
	"github.com/axllent/mailpit/internal/tools"
	"github.com/google/uuid"
	"github.com/jhillyerd/enmime/v2"
)

// SendMessageHandler handles HTTP requests to send a new message
func SendMessageHandler(w http.ResponseWriter, r *http.Request) {
	// swagger:route POST /api/v1/send message SendMessageParams
	//
	// # Send a message
	//
	// Send a message via the HTTP API.
	//
	//	Consumes:
	//	  - application/json
	//
	//	Produces:
	//	  - application/json
	//
	//	Schemes: http, https
	//
	//	Responses:
	//	  200: SendMessageResponse
	//	  400: JSONErrorResponse

	if config.DemoMode {
		httpJSONError(w, "this functionality has been disabled for demonstration purposes")
		return
	}

	if config.MaxMessageSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(config.MaxMessageSize)*1024*1024)
	}

	// Read body before decoding so that MaxBytesReader errors are returned directly.
	// In Go 1.26+, json.Decoder wraps reader errors in *json.SyntaxError, which
	// prevents errors.As from finding *http.MaxBytesError to return a 413.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
		httpJSONError(w, err.Error())
		return
	}

	data := sendMessageParams{}

	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&data.Body); err != nil {
		httpJSONError(w, err.Error())
		return
	}

	var httpAuthUser *string
	if user, _, ok := r.BasicAuth(); ok {
		httpAuthUser = &user
	}

	id, err := data.Send(r.RemoteAddr, httpAuthUser)

	if err != nil {
		httpJSONError(w, err.Error())
		return
	}

	w.Header().Add("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct{ ID string }{ID: id}); err != nil {
		httpError(w, err.Error())
	}
}

// Send will validate the message structure and attempt to send to Mailpit.
// It returns a sending summary or an error.
func (d sendMessageParams) Send(remoteAddr string, httpAuthUser *string) (string, error) {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return "", fmt.Errorf("error parsing request RemoteAddr: %s", err.Error())
	}

	ipAddr := &net.IPAddr{IP: net.ParseIP(ip)}

	addresses := []string{}

	msg := enmime.Builder().
		From(d.Body.From.Name, d.Body.From.Email).
		Subject(d.Body.Subject).
		Text([]byte(d.Body.Text))

	if d.Body.HTML != "" {
		d.Body.HTML = convertDataURIToCID(d.Body.HTML, func(data []byte, contentType, cid string) {
			ext := cidExt(contentType)
			msg = msg.AddInline(data, contentType, "inline"+ext, cid)
		})
		msg = msg.HTML([]byte(d.Body.HTML))
	}

	if len(d.Body.To) > 0 {
		for _, a := range d.Body.To {
			if _, err := mail.ParseAddress(a.Email); err == nil {
				msg = msg.To(a.Name, a.Email)
				addresses = append(addresses, a.Email)
			} else {
				return "", fmt.Errorf("invalid To address: %s", a.Email)
			}
		}
	}

	if len(d.Body.Cc) > 0 {
		for _, a := range d.Body.Cc {
			if _, err := mail.ParseAddress(a.Email); err == nil {
				msg = msg.CC(a.Name, a.Email)
				addresses = append(addresses, a.Email)
			} else {
				return "", fmt.Errorf("invalid Cc address: %s", a.Email)
			}
		}
	}

	if len(d.Body.Bcc) > 0 {
		for _, e := range d.Body.Bcc {
			if _, err := mail.ParseAddress(e); err == nil {
				msg = msg.BCC("", e)
				addresses = append(addresses, e)
			} else {
				return "", fmt.Errorf("invalid Bcc address: %s", e)
			}
		}
	}

	if len(d.Body.ReplyTo) > 0 {
		for _, a := range d.Body.ReplyTo {
			if _, err := mail.ParseAddress(a.Email); err == nil {
				msg = msg.ReplyTo(a.Name, a.Email)
			} else {
				return "", fmt.Errorf("invalid Reply-To address: %s", a.Email)
			}
		}
	}

	restrictedHeaders := []string{"To", "From", "Cc", "Bcc", "Reply-To", "Date", "Subject", "Content-Type", "Mime-Version"}

	if len(d.Body.Tags) > 0 {
		msg = msg.Header("X-Tags", strings.Join(d.Body.Tags, ", "))
		restrictedHeaders = append(restrictedHeaders, "X-Tags")
	}

	if len(d.Body.Headers) > 0 {
		for k, v := range d.Body.Headers {
			// check header isn't in "restricted" headers
			if tools.InArray(k, restrictedHeaders) {
				return "", fmt.Errorf("cannot overwrite header: \"%s\"", k)
			}
			msg = msg.Header(k, v)
		}
	}

	if len(d.Body.Attachments) > 0 {
		for _, a := range d.Body.Attachments {
			// workaround: split string because JS readAsDataURL() returns the base64 string
			// with the mime type prefix eg: data:image/png;base64,<base64String>
			parts := strings.Split(a.Content, ",")
			content := parts[len(parts)-1]
			b, err := base64.StdEncoding.DecodeString(content)
			if err != nil {
				return "", fmt.Errorf("error decoding base64 attachment \"%s\": %s", a.Filename, err.Error())
			}
			contentType := http.DetectContentType(b)
			if a.ContentType != "" {
				contentType = a.ContentType
			}
			if a.ContentID != "" {
				msg = msg.AddInline(b, contentType, a.Filename, a.ContentID)
			} else {
				msg = msg.AddAttachment(b, contentType, a.Filename)
			}
		}
	}

	part, err := msg.Build()
	if err != nil {
		return "", fmt.Errorf("error building message: %s", err.Error())
	}

	var buff bytes.Buffer

	if err := part.Encode(io.Writer(&buff)); err != nil {
		return "", fmt.Errorf("error building message: %s", err.Error())
	}

	return smtpd.SaveToDatabase(ipAddr, d.Body.From.Email, addresses, buff.Bytes(), httpAuthUser)
}

// convertDataURIToCID scans HTML for <img> tags with data URIs, decodes them,
// replaces the src with a cid: reference, and invokes addInline for each.
// It returns the modified HTML.
func convertDataURIToCID(html string, addInline func(data []byte, contentType, cid string)) string {
	if html == "" {
		return html
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return html
	}

	modified := false
	doc.Find("img[src]").Each(func(i int, s *goquery.Selection) {
		src, exists := s.Attr("src")
		if !exists || !strings.HasPrefix(src, "data:") {
			return
		}

		commaIdx := strings.Index(src, ",")
		if commaIdx < 0 {
			return
		}

		header := src[:commaIdx]
		data := src[commaIdx+1:]

		if !strings.Contains(header, ";base64") {
			return
		}

		contentType := ""
		if ctIdx := strings.Index(header, ":"); ctIdx >= 0 {
			ctPart := header[ctIdx+1:]
			if semiIdx := strings.Index(ctPart, ";"); semiIdx >= 0 {
				contentType = ctPart[:semiIdx]
			}
		}

		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return
		}

		if contentType == "" {
			contentType = http.DetectContentType(b)
		}

		cid := strings.ReplaceAll(uuid.New().String(), "-", "") + "@mailpit"

		s.SetAttr("src", "cid:"+cid)
		modified = true

		addInline(b, contentType, cid)
	})

	if modified {
		body := doc.Find("body")
		if body.Length() > 0 {
			html, err = body.Html()
			if err != nil {
				return html
			}
		} else {
			html, err = doc.Html()
			if err != nil {
				return html
			}
		}
	}

	return html
}

var cidExtMap = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/bmp":  ".bmp",
	"image/svg+xml": ".svg",
}

func cidExt(contentType string) string {
	if ext, ok := cidExtMap[contentType]; ok {
		return ext
	}
	return ".bin"
}
