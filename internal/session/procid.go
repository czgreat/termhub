package session

// ProcID identifies a process beyond its pid: a recycled pid has another
// creation time. Windows: 100ns ticks of the creation FILETIME; Linux: clock
// ticks since the epoch (boot time plus /proc/<pid>/stat starttime).
type ProcID struct {
	PID     uint32 `json:"pid"`
	Created int64  `json:"created"`
	Name    string `json:"name,omitempty"`
}
