// Package fixver maps the FIX versions mockfix supports to session
// BeginStrings, dictionaries and quickfixgo route keys (design doc,
// section 5.3).
package fixver

import (
	"path/filepath"

	"github.com/quickfixgo/quickfix"
)

// Versions as written in the config.
const (
	FIX42     = "FIX.4.2"
	FIX44     = "FIX.4.4"
	FIX50SP2  = "FIX.5.0SP2"
	Default   = FIX44
	applVer50 = "9" // ApplVerID of FIX.5.0SP2
)

// All lists the supported versions.
var All = []string{FIX42, FIX44, FIX50SP2}

// RouteKeys are the version keys of quickfix.MessageRouter: the
// BeginString, or the ApplVerID for FIXT sessions.
var RouteKeys = []string{quickfix.BeginStringFIX42, quickfix.BeginStringFIX44, applVer50}

// BeginString of a session of the given version (FIX 5.0 runs over FIXT 1.1).
func BeginString(version string) string {
	if version == FIX50SP2 {
		return quickfix.BeginStringFIXT11
	}
	return version
}

// Dictionaries returns the data dictionary files of a version, found next
// to the default FIX 4.4 dictionary: FIX42.xml, or FIXT11.xml (transport)
// and FIX50SP2.xml (application).
func Dictionaries(version, fix44Dict string) (app, transport string) {
	dir := filepath.Dir(fix44Dict)
	switch version {
	case FIX42:
		return filepath.Join(dir, "FIX42.xml"), ""
	case FIX50SP2:
		return filepath.Join(dir, "FIX50SP2.xml"), filepath.Join(dir, "FIXT11.xml")
	}
	return fix44Dict, ""
}

// Is42 reports whether a session's BeginString is FIX 4.2.
func Is42(beginString string) bool { return beginString == quickfix.BeginStringFIX42 }
