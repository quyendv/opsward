package notify

import (
	"fmt"
	"strings"
)

// Limits of the text: a chat message is read at a glance.
const (
	textEvents   = 5
	textLogLines = 10
)

// Text renders a notification for a chat: the alert, the workload, and what in the
// diagnosis is wrong (pods not ready, warning events, the crashed container's last lines,
// parts that could not be read).
func Text(n Notification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s*", n.Alert.Name)
	if n.Alert.Severity != "" {
		fmt.Fprintf(&b, " (%s)", n.Alert.Severity)
	}
	if n.Alert.Summary != "" {
		fmt.Fprintf(&b, ": %s", n.Alert.Summary)
	}
	b.WriteString("\n")
	if w := n.Workload; w != nil {
		fmt.Fprintf(&b, "%s %s/%s in %s\n", w.Kind, w.Namespace, w.Name, w.Environment)
	}
	if n.Error != "" {
		fmt.Fprintf(&b, "Not investigated: %s\n", n.Error)
		return b.String()
	}
	d, _ := n.Diagnosis.(map[string]any)
	status, _ := d["status"].(map[string]any)
	if r, ok := status["replicas"].(map[string]any); ok {
		fmt.Fprintf(&b, "%v/%v ready\n", num(r["ready"]), num(r["desired"]))
	}
	for _, p := range list(status["pods"]) {
		pod, _ := p.(map[string]any)
		if ready, _ := pod["ready"].(bool); ready && pod["waiting"] == nil {
			continue
		}
		var parts []string
		if w, _ := pod["waiting"].(string); w != "" {
			parts = append(parts, w)
		} else if ph, _ := pod["phase"].(string); ph != "" {
			parts = append(parts, ph)
		}
		parts = append(parts, fmt.Sprintf("%v restarts", num(pod["restarts"])))
		if lt, ok := pod["lastTerminated"].(map[string]any); ok {
			parts = append(parts, fmt.Sprintf("last %v (exit %v)", lt["reason"], num(lt["exitCode"])))
		}
		fmt.Fprintf(&b, "• %v: %s\n", pod["name"], strings.Join(parts, ", "))
	}
	shown := 0
	for _, e := range list(d["events"]) {
		ev, _ := e.(map[string]any)
		if ev["type"] != "Warning" || shown == textEvents {
			continue
		}
		shown++
		count := ""
		if c := num(ev["count"]); c != "" && c != "0" && c != "1" {
			count = " (x" + c + ")"
		}
		fmt.Fprintf(&b, "⚠ %v%s %v: %v\n", ev["reason"], count, ev["object"], ev["message"])
	}
	for _, l := range list(d["logs"]) {
		lg, _ := l.(map[string]any)
		lines := list(lg["lines"])
		if len(lines) == 0 {
			continue
		}
		if len(lines) > textLogLines {
			lines = lines[len(lines)-textLogLines:]
		}
		prev := ""
		if p, _ := lg["previous"].(bool); p {
			prev = ", before the last restart"
		}
		fmt.Fprintf(&b, "Logs of %v/%v%s:\n```\n", lg["pod"], lg["container"], prev)
		for _, line := range lines {
			fmt.Fprintf(&b, "%v\n", line)
		}
		b.WriteString("```\n")
		break // the first failing container; the rest are in the webhook's diagnosis
	}
	if u, ok := d["unavailable"].(map[string]any); ok {
		for part, why := range u {
			fmt.Fprintf(&b, "Unavailable, %s: %v\n", part, why)
		}
	}
	return b.String()
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// num prints a JSON number without a fraction.
func num(v any) string {
	switch n := v.(type) {
	case float64:
		return fmt.Sprintf("%.0f", n)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
