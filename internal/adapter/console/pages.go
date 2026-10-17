package console

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/tool"
)

type approvalsData struct {
	Pending []tool.ApprovalView // the caller may decide them now
	Mine    []tool.ApprovalView // the caller's own requests
	Others  []tool.ApprovalView // decided, expired, or decided by someone else
}

func (c *console) approvals(w http.ResponseWriter, r *http.Request, s *store.Session, p core.Principal) {
	c.renderApprovals(w, r, s, p, http.StatusOK, "", noticeFor(r.URL.Query()))
}

func noticeFor(q url.Values) string {
	if id := q.Get("decided"); id != "" {
		return "Request " + id + ": " + q.Get("as") + "."
	}
	return ""
}

func (c *console) renderApprovals(w http.ResponseWriter, r *http.Request, s *store.Session, p core.Principal, status int, msg, notice string) {
	pg := page{Title: "Approvals", Nav: "approvals", Principal: &p, Session: s, Error: msg, Notice: notice}
	var list []tool.ApprovalView
	if err := c.call(r, p, "list_approvals", map[string]any{}, &list); err != nil {
		if pg.Error == "" {
			pg.Error = c.message("list_approvals", err)
		}
		c.render(w, status, "approvals", pg)
		return
	}
	var d approvalsData
	me := core.NormalizeSubject(p.Subject)
	for _, a := range list {
		switch {
		case a.CanDecide:
			d.Pending = append(d.Pending, a)
		case a.Requester == me:
			d.Mine = append(d.Mine, a)
		default:
			d.Others = append(d.Others, a)
		}
	}
	pg.Data = d
	c.render(w, status, "approvals", pg)
}

func (c *console) decide(w http.ResponseWriter, r *http.Request, s *store.Session, p core.Principal) {
	id, decision := r.PostFormValue("id"), r.PostFormValue("decision")
	args := map[string]any{"id": id, "decision": decision, "reason": strings.TrimSpace(r.PostFormValue("reason"))}
	var out map[string]any
	if err := c.call(r, p, "decide_approval", args, &out); err != nil {
		c.renderApprovals(w, r, s, p, http.StatusUnprocessableEntity, c.message("decide_approval", err), "")
		return
	}
	as := map[string]string{"approve": "approved", "deny": "denied", "withdraw": "withdrawn"}[decision]
	http.Redirect(w, r, "/console/approvals?"+url.Values{"decided": {id}, "as": {as}}.Encode(), http.StatusSeeOther)
}

// auditFilters are the audit page's form, as typed.
type auditFilters struct {
	Since, Until                                  string // datetime-local values, UTC
	Subject, Tool, Status, Environment, Namespace string
}

type auditData struct {
	Filters  auditFilters
	Statuses []string
	Records  []audit.Record
	Note     string
	Next     string // the next page
}

// datetimeLocal is the format of an HTML datetime-local input.
const datetimeLocal = "2006-01-02T15:04"

func (c *console) audit(w http.ResponseWriter, r *http.Request, s *store.Session, p core.Principal) {
	q := r.URL.Query()
	f := auditFilters{
		Since: q.Get("since"), Until: q.Get("until"), Subject: q.Get("subject"), Tool: q.Get("tool"),
		Status: q.Get("status"), Environment: q.Get("environment"), Namespace: q.Get("namespace"),
	}
	d := auditData{Filters: f, Statuses: []string{
		audit.StatusOK, audit.StatusDenied, audit.StatusInvalid, audit.StatusError, audit.StatusUnknownTool, audit.StatusPending,
	}}
	pg := page{Title: "Audit", Nav: "audit", Principal: &p, Session: s, Data: &d}
	args := map[string]any{}
	for name, v := range map[string]string{"subject": f.Subject, "tool": f.Tool, "status": f.Status, "environment": f.Environment, "namespace": f.Namespace, "before": q.Get("before")} {
		if v = strings.TrimSpace(v); v != "" {
			args[name] = v
		}
	}
	for name, v := range map[string]string{"since": f.Since, "until": f.Until} {
		if v == "" {
			continue
		}
		t, err := time.Parse(datetimeLocal, v)
		if err != nil {
			pg.Error = name + ": not a date and time"
			c.render(w, http.StatusBadRequest, "audit", pg)
			return
		}
		args[name] = t.UTC().Format(time.RFC3339)
	}
	var out struct {
		Records []audit.Record `json:"records"`
		Next    string         `json:"next"`
		Note    string         `json:"note"`
	}
	if err := c.call(r, p, "search_audit", args, &out); err != nil {
		pg.Error = c.message("search_audit", err)
		c.render(w, http.StatusOK, "audit", pg)
		return
	}
	d.Records, d.Note = out.Records, out.Note
	if out.Next != "" {
		next := url.Values{}
		for k, v := range q {
			if k != "before" {
				next[k] = v
			}
		}
		next.Set("before", out.Next)
		d.Next = "/console/audit?" + next.Encode()
	}
	c.render(w, http.StatusOK, "audit", pg)
}

type youData struct {
	Grants   []policy.BindingView
	Approves []policy.BindingView
}

func (c *console) you(w http.ResponseWriter, _ *http.Request, s *store.Session, p core.Principal) {
	var d youData
	if c.opt.Naming != nil {
		d.Grants, d.Approves = c.opt.Naming(p)
	}
	c.render(w, http.StatusOK, "you", page{Title: "You", Nav: "you", Principal: &p, Session: s, Data: d})
}
