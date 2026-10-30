package engine

import (
	"encoding/json"

	"github.com/captain/ferry/internal/proto"
)

func (e *Engine) SoftwareConfig(target string, q *proto.SoftwareConfigRequest) (json.RawMessage, error) {
	var out json.RawMessage
	err := e.withTarget(target, func(s *session) error {
		r, err := s.call("swconfig", &proto.Request{SoftwareConfig: q})
		if err != nil {
			return err
		}
		if r.RC != proto.CodeOK {
			return &AgentError{Cmd: "swconfig", RC: r.RC, Msg: r.Msg}
		}
		out = r.Data
		return nil
	})
	return out, err
}
