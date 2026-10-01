package runner

import "encoding/json"

// Completion state is parsed while streaming, independently of bounded log retention.
type AgentStream struct {
	Output    *Limited
	line      []byte
	discard   bool
	Completed bool
}

func (s *AgentStream) Write(b []byte) (int, error) {
	n, e := s.Output.Write(b)
	for _, c := range b {
		if c == '\n' {
			if !s.discard {
				var event struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(s.line, &event) == nil {
					switch event.Type {
					case "turn.completed":
						s.Completed = true
					case "turn.failed", "error":
						s.Completed = false
					}
				}
			}
			s.line = s.line[:0]
			s.discard = false
			continue
		}
		if s.discard {
			continue
		}
		if len(s.line) >= 1<<20 {
			s.discard = true
			s.line = s.line[:0]
			continue
		}
		s.line = append(s.line, c)
	}
	return n, e
}
