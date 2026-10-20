package handler

import (
	"io/fs"
	"strings"
)

// walkTemplates visits every served template (email templates excepted: no
// CSP applies to a mail client).
func walkTemplates(f func(name, body string)) error {
	return fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "templates/email/") {
			return err
		}
		b, err := templateFS.ReadFile(p)
		if err != nil {
			return err
		}
		f(p, string(b))
		return nil
	})
}
