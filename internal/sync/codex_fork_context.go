package sync

import (
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// prepareCodexForkContexts shares identical source prefixes. Message-free turns
// still establish a boundary, independently of the merged transcript's order.
func prepareCodexForkContexts(state *db.CodexForkState, source []string) ([]int, func(parser.CodexMessageOrigin, int) (bool, bool)) {
	positions := make(map[db.CodexForkTurnContext]int, len(state.TurnContexts))
	for i, turn := range state.TurnContexts {
		positions[turn] = i + 1
	}
	sourceContexts := make([]int, len(source)+1)
	for i, id := range source {
		turn := db.CodexForkTurnContext{Parent: sourceContexts[i], TurnID: id}
		index, exists := positions[turn]
		if !exists {
			state.TurnContexts = append(state.TurnContexts, turn)
			index = len(state.TurnContexts)
			positions[turn] = index
		}
		sourceContexts[i+1] = index
	}
	child := make([]bool, len(state.TurnContexts)+1)
	for i, turn := range state.TurnContexts {
		_, inherited := state.ParentTurns[turn.TurnID]
		child[i+1] = child[turn.Parent] || !inherited
	}
	return sourceContexts, func(origin parser.CodexMessageOrigin, index int) (inherited, known bool) {
		if !state.ParentKnown || origin.Line == 0 {
			return false, false
		}
		if index > 0 && index < len(child) {
			return !child[index], true
		}
		if !origin.InTurn {
			return true, true
		}
		// A named turn identifies legacy source evidence. An idless turn alone
		// cannot prove which side of a missing message-free boundary it was on.
		if origin.TurnID != "" {
			_, inherited := state.ParentTurns[origin.TurnID]
			return inherited, true
		}
		return false, false
	}
}

// retainCodexForkContexts drops prefixes no published message references.
func retainCodexForkContexts(state *db.CodexForkState) {
	needed := make([]bool, len(state.TurnContexts)+1)
	for _, message := range state.Messages {
		for index := message.TurnContext; index > 0 && !needed[index]; index = state.TurnContexts[index-1].Parent {
			needed[index] = true
		}
	}
	remap := make([]int, len(needed))
	turns := make([]db.CodexForkTurnContext, 0, len(state.TurnContexts))
	for i, turn := range state.TurnContexts {
		if needed[i+1] {
			turn.Parent = remap[turn.Parent]
			turns = append(turns, turn)
			remap[i+1] = len(turns)
		}
	}
	for i := range state.Messages {
		state.Messages[i].TurnContext = remap[state.Messages[i].TurnContext]
	}
	state.TurnContexts = turns
}

// codexForkContextContains reports whether full retains every turn in part,
// in source order. Only an observed prefix can replace a shorter snapshot.
func codexForkContextContains(turns []db.CodexForkTurnContext, full, part int, memo map[[2]int]bool) bool {
	var path [][2]int
	var contains bool
	for full > 0 && part > 0 && full != part {
		pair := [2]int{full, part}
		if found, known := memo[pair]; known {
			contains = found
			break
		}
		path = append(path, pair)
		if turns[full-1].TurnID == turns[part-1].TurnID {
			part = turns[part-1].Parent
		}
		full = turns[full-1].Parent
	}
	contains = contains || part == 0 || full == part
	for _, pair := range path {
		memo[pair] = contains
	}
	return contains
}
