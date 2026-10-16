package redact

import (
	"strings"
	"testing"
)

func TestManifestsBlankSecretData(t *testing.T) {
	in := `apiVersion: v1
kind: ConfigMap
metadata: { name: settings }
data:
  mode: fast
---
apiVersion: v1
kind: Secret
metadata:
  name: api-tls
type: kubernetes.io/tls
data:
  tls.crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUM=
  tls.key: LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tCk1JSUV2
stringData:
  config.yaml: |
    endpoint: https://api.example.com
    apiSecretValue: c2VjcmV0
  plain: opensesame
---
kind: Secret
data: { a: YWJj }
`
	out := Manifests(in)
	for _, gone := range []string{"LS0t", "c2VjcmV0", "opensesame", "YWJj", "endpoint: https://api.example.com"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q survived:\n%s", gone, out)
		}
	}
	for _, kept := range []string{"mode: fast", "  tls.crt: " + Placeholder, "  config.yaml: " + Placeholder, "  plain: " + Placeholder, "data: " + Placeholder, "type: kubernetes.io/tls", "name: api-tls"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q missing:\n%s", kept, out)
		}
	}
}

func TestManifestsLeaveOtherFilesAlone(t *testing.T) {
	for _, in := range []string{
		"replicaCount: 2\ndata:\n  key: value\n",
		"# kind: Secret in a comment\ndata:\n  key: value\n",
	} {
		if got := Manifests(in); got != in {
			t.Errorf("Manifests changed %q to %q", in, got)
		}
	}
}
