package main

import (
	"fmt"
	"io"
	"log"
)

var (
	writeDaemonRuntimeWithAuthAndNoSync = WriteDaemonRuntimeWithAuthAndNoSync
	writeDaemonRuntimeWithAuth          = WriteDaemonRuntimeWithAuth
)

func warnRuntimeRecordWrite(
	out io.Writer, err error, context, remedy string,
) {
	warning := fmt.Sprintf(
		"warning: could not write daemon runtime record: %v (%s)",
		err, context,
	)
	log.Print(warning)
	fmt.Fprintln(out, warning)
	if remedy != "" {
		log.Print(remedy)
		fmt.Fprintln(out, remedy)
	}
}

func reportRuntimeRecordWrite(
	out io.Writer, err error, context, remedy string,
) {
	if err == nil {
		return
	}
	warnRuntimeRecordWrite(out, err, context, remedy)
}
