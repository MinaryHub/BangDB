//go:build tools

package bangdb

// gomobile bind needs golang.org/x/mobile/bind in this module's
// dependencies; this keeps "go mod tidy" from removing it.
import _ "golang.org/x/mobile/bind"
