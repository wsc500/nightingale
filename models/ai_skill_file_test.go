package models

import "testing"

func TestAISkillFileSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int64
		binary   bool
		accepted bool
	}{
		{"asset.bin", 500 * 1024 * 1024, true, true},
		{"asset.bin", 500*1024*1024 + 1, true, false},
		{"notes.txt", 16 * 1024 * 1024, false, true},
		{"notes.txt", 16*1024*1024 + 1, false, false},
		{"SKILL.md", 64 * 1024, false, true},
		{"SKILL.md", 64*1024 + 1, false, false},
		{"SKILL.md", 64*1024 + 1, true, false},
	} {
		if err := ValidateAISkillFileSize(tc.name, tc.size, tc.binary); (err == nil) != tc.accepted {
			t.Errorf("name=%s size=%d binary=%v: accepted=%v err=%v", tc.name, tc.size, tc.binary, tc.accepted, err)
		}
	}
}
