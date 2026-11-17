package main

import "testing"

func TestCountRounds(t *testing.T) {
	agent := &actor{"Bot", "fc-agent"}
	c := func(a *actor, body string) comment { return comment{body, a} }
	for _, tc := range []struct {
		name         string
		comments     []comment
		rounds, idle int
	}{
		{"none", nil, 0, 0},
		{"rounds", []comment{
			c(agent, roundMarker+"\n…"), c(agent, idleMarker+"\n…"), c(agent, idleMarker+"\n…"),
		}, 3, 2},
		{"progress ends the idle streak", []comment{
			c(agent, idleMarker+"\n…"), c(agent, roundMarker+"\n…"), c(agent, idleMarker+"\n…"),
		}, 3, 1},
		{"parking starts afresh", []comment{
			c(agent, idleMarker+"\n…"), c(agent, idleMarker+"\n…"), c(agent, parkMarker+"\n…"), c(agent, roundMarker+"\n…"),
		}, 1, 0},
		{"only the worker's own comments", []comment{
			c(&actor{"User", "mallory"}, idleMarker), c(&actor{"User", "fc-agent"}, idleMarker), c(nil, idleMarker),
		}, 0, 0},
		{"only markers at the start", []comment{
			c(agent, "> "+idleMarker), c(agent, "As the worker said: "+roundMarker),
		}, 0, 0},
	} {
		rounds, idle := countRounds(tc.comments, "fc-agent[bot]")
		if rounds != tc.rounds || idle != tc.idle {
			t.Errorf("%s: %d rounds, %d idle, want %d and %d", tc.name, rounds, idle, tc.rounds, tc.idle)
		}
	}
}
