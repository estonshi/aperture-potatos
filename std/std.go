// Package std installs the default extensions; each default is one line of defaults, so removing an extension is removing its line.
package std

import (
	"slices"

	"github.com/estonshi/aperture-potatos/ext/cmd/clearcmd"
	"github.com/estonshi/aperture-potatos/ext/cmd/compactcmd"
	"github.com/estonshi/aperture-potatos/ext/cmd/contextcmd"
	"github.com/estonshi/aperture-potatos/ext/cmd/forkcmd"
	"github.com/estonshi/aperture-potatos/ext/cmd/helpcmd"
	"github.com/estonshi/aperture-potatos/ext/cmd/rewindcmd"
	"github.com/estonshi/aperture-potatos/ext/cmd/usagecmd"
	"github.com/estonshi/aperture-potatos/ext/compact"
	"github.com/estonshi/aperture-potatos/ext/history"
	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/ext/tasks"
	"github.com/estonshi/aperture-potatos/kernel"
)

func defaults() []kernel.Extension {
	return []kernel.Extension{
		compact.New(compact.Options{}),
		history.New(),
		tasks.New(),
		subagent.New(subagent.Options{}),
		helpcmd.New(),
		usagecmd.New(),
		compactcmd.New(),
		clearcmd.New(clearcmd.Options{}),
		rewindcmd.New(rewindcmd.Options{}),
		forkcmd.New(),
		contextcmd.New(),
	}
}

type Opt func(*options)

type options struct {
	with    []kernel.Extension
	without []string
}

// With adds an extension, or replaces the default of the same name.
func With(exts ...kernel.Extension) Opt {
	return func(o *options) { o.with = append(o.with, exts...) }
}

// Without leaves out the extensions with these names.
func Without(names ...string) Opt {
	return func(o *options) { o.without = append(o.without, names...) }
}

// Defaults installs the default extensions at agent scope, adjusted by With and Without.
func Defaults(opts ...Opt) kernel.Option { return kernel.Use(Extensions(opts...)...) }

// Extensions is the list Defaults installs, for a caller that installs them another way.
func Extensions(opts ...Opt) []kernel.Extension {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	var exts []kernel.Extension
	for _, e := range defaults() {
		if !slices.ContainsFunc(o.with, func(w kernel.Extension) bool { return w.Name() == e.Name() }) {
			exts = append(exts, e)
		}
	}
	exts = append(exts, o.with...)
	return slices.DeleteFunc(exts, func(e kernel.Extension) bool { return slices.Contains(o.without, e.Name()) })
}
