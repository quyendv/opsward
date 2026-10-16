package main

import (
	"flag"
	"fmt"
	"io"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/quyendv/opsward/internal/app"
	"github.com/quyendv/opsward/internal/config"
)

const rbacUsage = `usage: opsward rbac render --config FILE [flags]

Print the Kubernetes RBAC each profile needs for what policy lets run through it.
Nothing is applied: review the output, then apply it with an administrator's credentials.

flags:
  --service-account-namespace NS   namespace of the default subjects, ServiceAccounts
                                   opsward-<profile> (default opsward)
  --subject PROFILE=KIND:NAME      bind PROFILE to a User, Group or ServiceAccount
                                   (NAME is NAMESPACE/NAME for a ServiceAccount); repeatable
`

func rbac(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "render" {
		return usageError(rbacUsage)
	}
	fs := flag.NewFlagSet("rbac render", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "")
	saNamespace := fs.String("service-account-namespace", "opsward", "")
	var subjects stringList
	fs.Var(&subjects, "subject", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" {
		return usageError(rbacUsage)
	}
	opt := app.RBACOptions{ServiceAccountNamespace: *saNamespace, Subjects: map[string]rbacv1.Subject{}}
	for _, s := range subjects {
		profile, subject, err := app.ParseRBACSubject(s)
		if err != nil {
			return err
		}
		opt.Subjects[profile] = subject
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	out, err := app.RenderRBAC(cfg, opt)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(stdout, out)
	return err
}
