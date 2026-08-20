package jsonutil

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

var durationMarshalers = json.MarshalToFunc(func(out *jsontext.Encoder, duration time.Duration) error {
	return out.WriteToken(jsontext.Int(int64(duration)))
})

var durationUnmarshalers = json.UnmarshalFromFunc(func(in *jsontext.Decoder, duration *time.Duration) error {
	var nanoseconds int64
	if err := json.UnmarshalDecode(in, &nanoseconds); err != nil {
		return err
	}
	*duration = time.Duration(nanoseconds)
	return nil
})

func MarshalDurationFields[T any](out *jsontext.Encoder, value T) error {
	return json.MarshalEncode(out, value, json.WithMarshalers(durationMarshalers))
}

func UnmarshalDurationFields[T any](in *jsontext.Decoder, value *T) error {
	return json.UnmarshalDecode(in, value, json.WithUnmarshalers(durationUnmarshalers))
}
