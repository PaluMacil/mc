package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePlayerList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    string
		want  OnlinePlayers
		valid bool
	}{
		{"one player", "There are 1 of a max of 10 players online: msmborders",
			OnlinePlayers{Online: 1, Max: 10, Names: []string{"msmborders"}}, true},
		{"none", "There are 0 of a max of 10 players online: ",
			OnlinePlayers{Online: 0, Max: 10}, true},
		{"several", "There are 3 of a max of 20 players online: a, b, c",
			OnlinePlayers{Online: 3, Max: 20, Names: []string{"a", "b", "c"}}, true},
		{"section colors", "§eThere are 1 of a max of 10 players online:§r msmborders",
			OnlinePlayers{Online: 1, Max: 10, Names: []string{"msmborders"}}, true},
		{"ansi trailing", "There are 1 of a max of 10 players online: msmborders\x1b[0m",
			OnlinePlayers{Online: 1, Max: 10, Names: []string{"msmborders"}}, true},
		{"unexpected", "unknown command", OnlinePlayers{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePlayerList(tt.in)
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
