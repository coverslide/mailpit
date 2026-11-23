package apiv1

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestConvertDataURIToCID(t *testing.T) {
	content := []byte("inline image")
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(content)
	var gotData []byte
	var gotType, gotCID string

	gotHTML := convertDataURIToCID(`<p>before</p><img alt="logo" src="`+dataURI+`"><p>after</p>`, func(data []byte, contentType, cid string) {
		gotData = data
		gotType = contentType
		gotCID = cid
	})

	if string(gotData) != string(content) || gotType != "image/png" || !strings.HasSuffix(gotCID, "@mailpit") {
		t.Fatalf("inline attachment = (%q, %q, %q)", gotData, gotType, gotCID)
	}
	if !strings.Contains(gotHTML, `src="cid:`+gotCID+`"`) || !strings.Contains(gotHTML, `alt="logo"`) {
		t.Fatalf("converted HTML did not preserve the image attributes: %s", gotHTML)
	}
}
