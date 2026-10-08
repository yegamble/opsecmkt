package market

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestMessageConversationFilterIsPrivateExactAndPaged(t *testing.T) {
	e := newTestApp(t)
	viewer, session := e.user("conversation_viewer", "buyer")
	partner, _ := e.user("ConversationPeer", "vendor")
	other, _ := e.user("conversation_other", "buyer")
	outsider, _ := e.user("conversation_outsider", "buyer")
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,created)
  SELECT lpad(to_hex(g),32,'0'), CASE WHEN g%2=0 THEN $1 ELSE $2 END,
  CASE WHEN g%2=0 THEN $2 ELSE $1 END, 'conversation-msg-'||lpad(g::text,3,'0'),true,now()
  FROM generate_series(1,101) g`, viewer, partner)
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted,created)
  SELECT lpad(to_hex(1000+g),32,'0'),$1,$2,'other-own-message-'||g,true,now()
  FROM generate_series(1,3) g`, viewer, other)
	agExec(e, `INSERT INTO messages(id,sender_id,recipient_id,body,encrypted) VALUES('00000000000000000000000000002000',$1,$2,'private-outsider-message',true)`, outsider, partner)
	for i, want := range []int{50, 50, 1} {
		path := "/messages?to=ConversationPeer&page=" + fmt.Sprint(i+1)
		page := e.body("GET", path, session, nil, 200)
		if got := strings.Count(page, "conversation-msg-"); got != want {
			t.Fatalf("page %d: %d messages, want %d", i+1, got, want)
		}
		mustContain(t, page, "Showing your correspondence with ConversationPeer", `href="/messages">Show all correspondence`, fmt.Sprintf("conversation-msg-%03d", 101-i*50), "of 101 messages")
		mustNotContain(t, page, "other-own-message-", "private-outsider-message")
		if i < 2 {
			mustContain(t, page, fmt.Sprintf(`href="/messages?page=%d&amp;to=ConversationPeer"`, i+2))
		}
	}
	for _, handle := range []string{"conversationpeer", "missing_peer", "conversation_viewer"} {
		page := e.body("GET", "/messages?to="+url.QueryEscape(handle), session, nil, 200)
		mustContain(t, page, "No messages yet", "Showing your correspondence with "+handle)
		mustNotContain(t, page, "conversation-msg-", "other-own-message-", "private-outsider-message")
	}
	all := e.body("GET", "/messages", session, nil, 200)
	mustContain(t, all, "other-own-message-", "of 104 messages")
	mustNotContain(t, all, "private-outsider-message", "Showing your correspondence with")
	invalid := e.body("GET", "/messages?to="+url.QueryEscape("ConversationPeer<script>"), session, nil, 200)
	mustContain(t, invalid, "other-own-message-", "of 104 messages")
	mustNotContain(t, invalid, "private-outsider-message", "Showing your correspondence with", "ConversationPeer&lt;script&gt;")
}
