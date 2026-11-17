# Vendored bubbletea v1.3.10

Upstream `github.com/charmbracelet/bubbletea` v1.3.10 (MIT), wired in via the
`replace` directive in the root `go.mod`. Only the module sources are kept;
upstream tests, CI and docs were dropped.

The single local change is in `standard_renderer.go`: inline-image support.
A frame line that carries an OSC 1337 image escape followed by the marker
`"\x00IMG:<rows>:<col>\x00"` is painted as its prefix only; the escape is
written after the frame so the image lands on cells the renderer will not
overwrite, and an image that moved or disappeared has its old rectangle
erased. Diff against upstream to see the patch:

    diff -r $(go env GOMODCACHE)/github.com/charmbracelet/bubbletea@v1.3.10 third_party/bubbletea
