package qbt

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Torrent is the subset of /torrents/info fields the replicator uses.
type Torrent struct {
	Hash                string  `json:"hash"`
	Name                string  `json:"name"`
	Size                int64   `json:"size"`
	TotalSize           int64   `json:"total_size"`
	Progress            float64 `json:"progress"`
	State               string  `json:"state"`
	Category            string  `json:"category"`
	RawTags             string  `json:"tags"`
	SavePath            string  `json:"save_path"`
	ContentPath         string  `json:"content_path"`
	AddedOn             int64   `json:"added_on"`
	CompletionOn        int64   `json:"completion_on"`
	Ratio               float64 `json:"ratio"`
	RatioLimit          float64 `json:"ratio_limit"`
	SeedingTimeLimit    int64   `json:"seeding_time_limit"`
	InactiveSeedingTime int64   `json:"inactive_seeding_time_limit"`
	UpSpeed             int64   `json:"upspeed"`
	DlSpeed             int64   `json:"dlspeed"`
	Uploaded            int64   `json:"uploaded"`
	Tracker             string  `json:"tracker"`
	NumLeechs           int     `json:"num_leechs"`
	NumSeeds            int     `json:"num_seeds"`
	AutoTMM             bool    `json:"auto_tmm"`
	// Private is only reported by qBittorrent >= 5.0; nil otherwise.
	Private *bool `json:"private,omitempty"`
}

// Tags parses the comma-separated tag list.
func (t Torrent) Tags() []string {
	if strings.TrimSpace(t.RawTags) == "" {
		return nil
	}
	parts := strings.Split(t.RawTags, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// HasTag reports whether the torrent carries tag (case-sensitive like qBittorrent).
func (t Torrent) HasTag(tag string) bool {
	for _, x := range t.Tags() {
		if x == tag {
			return true
		}
	}
	return false
}

// CompletedAt returns the completion time, zero if unknown.
func (t Torrent) CompletedAt() time.Time {
	if t.CompletionOn <= 0 {
		return time.Time{}
	}
	return time.Unix(t.CompletionOn, 0)
}

// Torrent states (4.x and 5.x names).
const (
	StateUploading   = "uploading"
	StateStalledUP   = "stalledUP"
	StateQueuedUP    = "queuedUP"
	StatePausedUP    = "pausedUP"
	StateStoppedUP   = "stoppedUP"
	StateForcedUP    = "forcedUP"
	StateCheckingUP  = "checkingUP"
	StateMoving      = "moving"
	StateMissing     = "missingFiles"
	StateError       = "error"
	StateCheckingDL  = "checkingDL"
	StateCheckResume = "checkingResumeData"
)

// IsSeedingState reports whether the state means "complete and settled".
func IsSeedingState(s string) bool {
	switch s {
	case StateUploading, StateStalledUP, StateQueuedUP, StatePausedUP, StateStoppedUP, StateForcedUP:
		return true
	}
	return false
}

// IsErrorState reports missing files / error states.
func IsErrorState(s string) bool { return s == StateMissing || s == StateError }

// IsStoppedState reports paused/stopped states (either dialect).
func IsStoppedState(s string) bool {
	return strings.HasPrefix(s, "paused") || strings.HasPrefix(s, "stopped")
}

// File is one entry of /torrents/files.
type File struct {
	Index    *int    `json:"index,omitempty"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
}

// Idx returns the file index.
func (f File) Idx() int {
	if f.Index == nil {
		return -1
	}
	return *f.Index
}

// Category is one entry of /torrents/categories.
type Category struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// TransferInfo is /transfer/info.
type TransferInfo struct {
	DlSpeed          int64  `json:"dl_info_speed"`
	UpSpeed          int64  `json:"up_info_speed"`
	DlData           int64  `json:"dl_info_data"`
	UpData           int64  `json:"up_info_data"`
	ConnectionStatus string `json:"connection_status"`
	DHTNodes         int64  `json:"dht_nodes"`
}

// Version is a WebAPI semantic version.
type Version [3]int

// ParseVersion parses "2.11.4" (missing parts are zero, garbage yields zero).
func ParseVersion(s string) Version {
	var v Version
	for i, p := range strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}
		}
		v[i] = n
	}
	return v
}

// Less compares versions.
func (v Version) Less(o Version) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// IsZero reports an unknown version.
func (v Version) IsZero() bool { return v == Version{} }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
