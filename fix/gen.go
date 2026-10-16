// Package fix holds the Go packages generated from spec/FIX44.xml
// (fix44, field, tag, enum). They are not committed; run `make gen`
// or `go generate ./fix` after cloning or after editing the spec.
package fix

//go:generate rm -rf fix44 field tag enum
//go:generate go tool generate-fix -pkg-root github.com/quynv/mock-fixser/fix ../spec/FIX44.xml
