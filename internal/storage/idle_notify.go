package storage

import (
	"fmt"
	"net"
	"sync"

	"github.com/axllent/mailpit/internal/logger"
)

// IdleConn represents a single connection currently in the IDLE state.
type IdleConn struct {
	conn    net.Conn
	mailbox string // IMAP mailbox name (e.g. "INBOX", "Sent")
	user    string
	notify  chan string // untagged IMAP responses to send during idle
	done    chan struct{}
}

// Done returns a channel that is closed when IDLE ends.
func (ic *IdleConn) Done() <-chan struct{} {
	return ic.done
}

// Notify returns the notification channel for receiving IMAP responses during IDLE.
func (ic *IdleConn) Notify() <-chan string {
	return ic.notify
}

// idleKey returns the map key for a user+mailbox pair.
func idleKey(user, mailbox string) string {
	return user + "\x00" + mailbox
}

// idleNotifier tracks all IDLEing connections and can notify them of changes.
type idleNotifier struct {
	mu    sync.RWMutex
	conns map[string][]*IdleConn // "user\x00mailbox" → IDLEing connections
}

var IdleNotify = &idleNotifier{
	conns: make(map[string][]*IdleConn),
}

// RegisterIdle adds a connection to the idle tracking for the given mailbox and user.
// Returns an IdleConn that can be used to receive notifications.
func (n *idleNotifier) RegisterIdle(conn net.Conn, mailbox, user string) *IdleConn {
	ic := &IdleConn{
		conn:    conn,
		mailbox: mailbox,
		user:    user,
		notify:  make(chan string, 8),
		done:    make(chan struct{}),
	}
	key := idleKey(user, mailbox)
	n.mu.Lock()
	n.conns[key] = append(n.conns[key], ic)
	n.mu.Unlock()
	logger.Log().Debugf("[imap] idle-notify: registered %s on %s (user=%s)", conn.RemoteAddr(), mailbox, user)
	return ic
}

// UnregisterIdle removes a connection from idle tracking and closes its done channel.
func (n *idleNotifier) UnregisterIdle(ic *IdleConn) {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := idleKey(ic.user, ic.mailbox)
	conns := n.conns[key]
	for i, c := range conns {
		if c == ic {
			n.conns[key] = append(conns[:i], conns[i+1:]...)
			break
		}
	}
	if len(n.conns[key]) == 0 {
		delete(n.conns, key)
	}
	select {
	case <-ic.done:
	default:
		close(ic.done)
	}
	logger.Log().Debugf("[imap] idle-notify: unregistered %s from %s (user=%s)", ic.conn.RemoteAddr(), ic.mailbox, ic.user)
}

// NotifyMailboxChangeUser sends an EXISTS notification to IDLEing clients
// for a specific user on the given mailbox with the updated message count.
func (n *idleNotifier) NotifyMailboxChangeUser(user, mailbox string, count int) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	response := fmt.Sprintf("* %d EXISTS", count)
	key := idleKey(user, mailbox)
	for _, ic := range n.conns[key] {
		select {
		case ic.notify <- response:
		default:
			logger.Log().Debugf("[imap] idle-notify: dropped notification for %s (channel full)", ic.conn.RemoteAddr())
		}
	}
}

// NotifyMailboxChange sends an EXISTS notification to all IDLEing clients
// on the given mailbox (any user) with the updated message count.
func (n *idleNotifier) NotifyMailboxChange(mailbox string, count int) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	response := fmt.Sprintf("* %d EXISTS", count)
	prefix := "\x00" + mailbox
	for key, conns := range n.conns {
		if len(key) > len(prefix) && key[len(key)-len(prefix):] == prefix {
			for _, ic := range conns {
				select {
				case ic.notify <- response:
				default:
					logger.Log().Debugf("[imap] idle-notify: dropped notification for %s (channel full)", ic.conn.RemoteAddr())
				}
			}
		}
	}
}

// NotifyFlagChange sends a FETCH FLAGS notification to all IDLEing clients
// for a specific user on the given mailbox for a specific message (by sequence number).
func (n *idleNotifier) NotifyFlagChangeUser(user, mailbox string, seqNum int, flags []string) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	flagStr := ""
	for i, f := range flags {
		if i > 0 {
			flagStr += " "
		}
		flagStr += f
	}
	response := fmt.Sprintf("* %d FETCH (FLAGS (%s))", seqNum, flagStr)
	key := idleKey(user, mailbox)
	for _, ic := range n.conns[key] {
		select {
		case ic.notify <- response:
		default:
			logger.Log().Debugf("[imap] idle-notify: dropped flag notification for %s", ic.conn.RemoteAddr())
		}
	}
}

// NotifyFlagChange sends a FETCH FLAGS notification to all IDLEing clients
// on the given mailbox for a specific message (by sequence number).
func (n *idleNotifier) NotifyFlagChange(mailbox string, seqNum int, flags []string) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	flagStr := ""
	for i, f := range flags {
		if i > 0 {
			flagStr += " "
		}
		flagStr += f
	}
	response := fmt.Sprintf("* %d FETCH (FLAGS (%s))", seqNum, flagStr)
	prefix := "\x00" + mailbox
	for key, conns := range n.conns {
		if len(key) > len(prefix) && key[len(key)-len(prefix):] == prefix {
			for _, ic := range conns {
				select {
				case ic.notify <- response:
				default:
					logger.Log().Debugf("[imap] idle-notify: dropped flag notification for %s", ic.conn.RemoteAddr())
				}
			}
		}
	}
}

// NotifyExpunge sends an EXPUNGE notification to all IDLEing clients
// on the given mailbox for a specific sequence number.
func (n *idleNotifier) NotifyExpunge(mailbox string, seqNum int) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	response := fmt.Sprintf("* %d EXPUNGE", seqNum)
	prefix := "\x00" + mailbox
	for key, conns := range n.conns {
		if len(key) > len(prefix) && key[len(key)-len(prefix):] == prefix {
			for _, ic := range conns {
				select {
				case ic.notify <- response:
				default:
					logger.Log().Debugf("[imap] idle-notify: dropped expunge notification for %s", ic.conn.RemoteAddr())
				}
			}
		}
	}
}

// FolderToMailbox maps a storage folder name to an IMAP mailbox name.
// Empty string (default inbox) maps to "INBOX".
func FolderToMailbox(folder string) string {
	if folder == "" {
		return "INBOX"
	}
	return folder
}

// NotifyFolderChangeUser is a convenience wrapper that converts a storage folder
// name to an IMAP mailbox name and sends an EXISTS notification for a specific user.
func (n *idleNotifier) NotifyFolderChangeUser(user, folder string, count int) {
	n.NotifyMailboxChangeUser(user, FolderToMailbox(folder), count)
}

// NotifyFolderChange is a convenience wrapper that converts a storage folder
// name to an IMAP mailbox name and sends an EXISTS notification to all users.
func (n *idleNotifier) NotifyFolderChange(folder string, count int) {
	n.NotifyMailboxChange(FolderToMailbox(folder), count)
}
