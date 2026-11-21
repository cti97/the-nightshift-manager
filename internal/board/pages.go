// internal/board/pages.go
package board

import (
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

// This file is the operator's daily screen (M3 D8). One route, two
// representations: the approvals page when the client asks for HTML, the JSON
// the API already answered when it asks for JSON. A phone gets a single column,
// a form per item, and no JavaScript — a page that needs a script to approve
// work is a page that cannot be trusted from a locked-down phone.

// approvalsPage is the queue. Every field is rendered as text (html/template
// escapes it), the note is optional, and nothing on this page writes anywhere but
// the decision route the buttons post to.
var approvalsPage = template.Must(template.New("approvals").Funcs(template.FuncMap{
	// when renders the board's seconds as a fixed UTC stamp. The operator is
	// comparing "how long has this waited" against the item, not reading a
	// locale-sensitive date.
	"when": func(ts int64) string {
		return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04") + " UTC"
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Approvals — the night shift manager</title>
<style>
  body { margin: 0 auto; max-width: 44rem; padding: 1rem; font: 16px/1.5 system-ui, -apple-system, sans-serif; color: #111; background: #fff; }
  section { display: block; border: 1px solid #9994; border-radius: .5rem; padding: 1rem; margin: 1rem 0; }
  h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
  h2 { font-size: 1.1rem; margin: 0 0 .5rem; }
  .meta { font-size: .85rem; color: #555; margin: .25rem 0; }
  .action { font-weight: 600; margin: .5rem 0; }
  .empty { color: #555; }
  form { margin-top: .75rem; }
  label { display: block; font-size: .9rem; color: #333; }
  textarea { width: 100%; box-sizing: border-box; font: inherit; margin-top: .25rem; }
  button { font: inherit; padding: .5rem 1.1rem; margin: .5rem .5rem 0 0; border-radius: .35rem; border: 1px solid #777; background: #f4f4f4; }
  button[value="deny"] { background: #fff; }
</style>
</head>
<body>
<h1>Approvals</h1>
<p class="meta">{{len .}} waiting for a decision</p>
<main>
{{if not .}}<p class="empty">Nothing is waiting for a decision.</p>{{end}}
{{range .}}
<section class="card" id="item-{{.ItemID}}">
  <h2>{{.Title}}</h2>
  {{if .Action}}<p class="action">{{.Action}}</p>{{else}}<p class="action">(no action was recorded on this request)</p>{{end}}
  {{if .Reason}}<p>{{.Reason}}</p>{{end}}
  {{if .Options}}<ul>{{range .Options}}<li>{{.}}</li>{{end}}</ul>{{end}}
  <p class="meta">asked by {{.RequestedBy}} at {{when .RequestedAt}} — attempt {{.Attempt}}</p>
  <form method="post" action="/admin/approvals/{{.ItemID}}">
    <label>Note, recorded with your decision
      <textarea name="note" rows="2"></textarea>
    </label>
    <button type="submit" name="decision" value="approve">Approve</button>
    <button type="submit" name="decision" value="deny">Deny</button>
  </form>
</section>
{{end}}
</main>
</body>
</html>
`))

// decisionErrorPage is what a phone sees when a form post is refused — a second
// tap on an item someone already decided, most often. Plain text with a way back,
// because a JSON envelope is unreadable in a browser.
var decisionErrorPage = template.Must(template.New("decision-error").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Decision refused</title>
<style>
  body { margin: 0 auto; max-width: 44rem; padding: 1rem; font: 16px/1.5 system-ui, sans-serif; color: #111; background: #fff; }
  .error { border: 1px solid #b33; border-radius: .5rem; padding: 1rem; margin: 1rem 0; }
</style>
</head>
<body>
<h1>Decision refused</h1>
<p class="error">{{.Message}}</p>
<p><a href="/admin/approvals">Back to approvals</a></p>
</body>
</html>
`))

// wantsHTML decides the representation from Accept, first declared first served.
// A browser opens with text/html; curl, the CLI and the API send application/json
// or */*, and get JSON — the machine answer, which is also the one that existed
// before this page did.
func wantsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch media {
		case "text/html", "application/xhtml+xml":
			return true
		case "application/json":
			return false
		}
	}
	return false
}

// isFormPost is the page's transport test. The decision route accepts both: a
// form-encoded body answers in HTML and redirects, a JSON body answers in JSON.
func isFormPost(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
}

// renderHTML writes one page. The status must be set before the template runs,
// because a template writes bytes as it goes; a rendering failure after that can
// only be logged, and the partial page is what the caller gets.
func renderHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("board: render page: %v", err)
	}
}

// threadPage is the conversation with its budget bar (spec §14 item 4). The bar is a plain
// div with a width, not a canvas: a phone must be able to read it, and the numbers beside it
// are what an operator actually quotes.
var threadPage = template.Must(template.New("thread").Funcs(template.FuncMap{
	"when": func(ts int64) string {
		return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04") + " UTC"
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Subject}} — thread</title>
<style>
  body { margin: 0 auto; max-width: 44rem; padding: 1rem; font: 16px/1.5 system-ui, -apple-system, sans-serif; color: #111; background: #fff; }
  h1 { font-size: 1.3rem; margin: 0 0 .25rem; }
  .meta { font-size: .85rem; color: #555; margin: .25rem 0; }
  .bar { height: .75rem; border-radius: .4rem; background: #e8e8e8; overflow: hidden; margin: .5rem 0 .25rem; }
  .bar span { display: block; height: 100%; background: #444; }
  .bar span.frozen { background: #b33; }
  ol { list-style: none; padding: 0; }
  li { border: 1px solid #9994; border-radius: .5rem; padding: .6rem .8rem; margin: .6rem 0; }
  .author { font-size: .8rem; color: #555; text-transform: uppercase; letter-spacing: .03em; }
  .body { white-space: pre-wrap; margin: .25rem 0 0; }
  form { margin-top: 1rem; }
  label { display: block; font-size: .9rem; color: #333; }
  textarea { width: 100%; box-sizing: border-box; font: inherit; margin-top: .25rem; }
  button { font: inherit; padding: .5rem 1.1rem; margin-top: .5rem; border-radius: .35rem; border: 1px solid #777; background: #f4f4f4; }
  .closed { color: #555; }
</style>
</head>
<body>
<h1>{{.Subject}}</h1>
<p class="meta">{{.Status}} — {{.TokensUsed}} of {{.BudgetTokens}} tokens used, {{.Remaining}} left{{if .TurnOwner}} — last word from {{.TurnOwner}}{{end}}</p>
<div class="bar"><span class="{{.Status}}" style="width:{{.UsedPct}}%"></span></div>
{{if .WorkItemID}}<p class="meta">item {{.WorkItemID}}</p>{{end}}
<ol>
{{range .Messages}}
<li>
  <div class="author">{{.Author}} — {{when .CreatedAt}} — {{.TokensCharged}} tokens</div>
  <div class="body">{{.Body}}</div>
</li>
{{end}}
</ol>
{{if eq .Status "open"}}
<form method="post" action="/admin/threads/{{.ID}}/messages">
  <label>Your reply, as the operator
    <textarea name="body" rows="3" required></textarea>
  </label>
  <button type="submit">Post</button>
</form>
{{else}}<p class="closed">This thread is {{.Status}}; only raising its budget can reopen it.</p>{{end}}
</body>
</html>
`))

// threadErrorPage is what a browser sees when its post is refused — a frozen thread, most
// often — with the reason and a way back.
var threadErrorPage = template.Must(template.New("thread-error").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Post refused</title>
<style>body { margin: 0 auto; max-width: 44rem; padding: 1rem; font: 16px/1.5 system-ui, sans-serif; }
.error { border: 1px solid #b33; border-radius: .5rem; padding: 1rem; margin: 1rem 0; }</style></head>
<body><h1>Post refused</h1><p class="error">{{.Message}}</p>
<p><a href="/admin/threads/{{.ID}}">Back to the thread</a></p></body></html>
`))

// threadPageData is the view the template needs: the thread, its messages, and the bar's
// percentage, clamped at 100 because an overspent thread (settlement is uncapped, D1) must
// still draw a full bar rather than an overhanging one.
type threadPageData struct {
	ThreadView
	UsedPct int
}

func newThreadPageData(view ThreadView) threadPageData {
	pct := 0
	if view.BudgetTokens > 0 {
		pct = view.TokensUsed * 100 / view.BudgetTokens
	}
	if pct > 100 {
		pct = 100
	}
	return threadPageData{ThreadView: view, UsedPct: pct}
}

// handleThreadPage is GET /admin/threads/{id}: the page for a browser, the JSON view for
// everything else. One route, like the approvals page, so the two representations cannot
// diverge in what they count.
func (s *Server) handleThreadPage(w http.ResponseWriter, r *http.Request) {
	view, err := GetThread(r.Context(), s.db, r.PathValue("id"))
	if err != nil {
		status, code := threadErrorStatus(err)
		if wantsHTML(r) {
			renderHTML(w, status, threadErrorPage, map[string]string{
				"ID": r.PathValue("id"), "Message": err.Error(),
			})
			return
		}
		WriteError(w, status, code, err.Error())
		return
	}
	if wantsHTML(r) {
		renderHTML(w, http.StatusOK, threadPage, newThreadPageData(view))
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

// handleAdminMessage is POST /admin/threads/{id}/messages: the operator's own reply. The
// author is "admin" and only "admin" — the route is behind the admin credential, so letting a
// body name an author would let an operator impersonate an agent in the thread's record.
// That is also the only path by which D3's exemption (an operator may post twice) exists.
func (s *Server) handleAdminMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req PostMessageRequest
	if isFormPost(r) {
		if err := r.ParseForm(); err != nil {
			WriteError(w, http.StatusBadRequest, "bad_form", err.Error())
			return
		}
		req = PostMessageRequest{Author: "admin", Body: r.PostFormValue("body")}
	} else if !decodeJSON(w, r, &req) {
		return
	} else {
		req.Author = "admin"
	}
	res, err := PostMessage(r.Context(), s.db, s.clock, id, req)
	if err != nil {
		status, code := threadErrorStatus(err)
		if isFormPost(r) {
			renderHTML(w, status, threadErrorPage, map[string]string{"ID": id, "Message": err.Error()})
			return
		}
		WriteError(w, status, code, err.Error())
		return
	}
	if isFormPost(r) {
		http.Redirect(w, r, "/admin/threads/"+id, http.StatusSeeOther)
		return
	}
	WriteJSON(w, http.StatusCreated, res)
}
