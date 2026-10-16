package fixserver

import (
	"fmt"
	"log/slog"

	"github.com/quickfixgo/quickfix"
)

// slogLogFactory routes quickfixgo session events to slog at debug level.
// Raw message logs are left to the monitor mirror (phase 4).
type slogLogFactory struct{ log *slog.Logger }

func (f slogLogFactory) Create() (quickfix.Log, error) {
	return slogLog{f.log.With("component", "quickfix")}, nil
}

func (f slogLogFactory) CreateSessionLog(id quickfix.SessionID) (quickfix.Log, error) {
	return slogLog{f.log.With("component", "quickfix", "session", id.String())}, nil
}

type slogLog struct{ log *slog.Logger }

func (slogLog) OnIncoming([]byte)                     {}
func (slogLog) OnOutgoing([]byte)                     {}
func (l slogLog) OnEvent(s string)                    { l.log.Debug(s) }
func (l slogLog) OnEventf(format string, args ...any) { l.log.Debug(fmt.Sprintf(format, args...)) }
