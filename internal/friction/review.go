package friction

// Review runs every detector over one session: corrections, errors,
// workarounds, deferrals, patterns (jilog's run order,
// digest.rs:425-437), then frustration and interruptions (spec §6.8).
// A persona selects the chat correction detector and nothing else does
// (spec §6.2). Dims are stamped on every signal, with persona and
// channel only when a persona is set (digest.rs:385-463), SubjectKind
// is SubjectSession, and Seq records the run order. Excluded sessions
// yield nil.
func Review(in SessionInput) []Signal {
	if in.Excluded {
		return nil
	}
	var out []Signal
	if in.Dims.Persona != "" {
		out = append(out, DetectCorrectionsChat(in.Messages, in.SubjectID)...)
	} else {
		out = append(out, DetectCorrections(in.Messages, in.SubjectID)...)
	}
	out = append(out, DetectErrors(in.Messages, in.SubjectID)...)
	out = append(out, DetectWorkarounds(in.Messages, in.SubjectID)...)
	out = append(out, DetectDeferrals(in.Messages, in.SubjectID)...)
	out = append(out, DetectPatterns(in.SubjectID, in.IsSubAgent, in.Patterns)...)
	out = append(out, DetectFrustration(in.Messages, in.SubjectID)...)
	out = append(out, DetectInterruptions(in.Interruptions, in.SubjectID)...)

	dims := in.Dims
	if dims.Persona == "" {
		dims.Channel = ""
	}
	for i := range out {
		out[i].Dims = dims
		out[i].SubjectKind = SubjectSession
		out[i].Seq = i
	}
	return out
}
