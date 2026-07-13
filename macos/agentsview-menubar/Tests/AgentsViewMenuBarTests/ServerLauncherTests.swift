import Foundation
import Testing
@testable import AgentsViewMenuBar

@Test("starts the server with the detached no-browser command")
func startUsesDetachedServerCommand() throws {
    #expect(
        ServerLauncher.startArguments == [
            "serve", "--background", "--no-browser", "--port", "0",
        ]
    )

    var calls: [[String]] = []
    let launcher = ServerLauncher(command: ["agentsview"]) { arguments in
        calls.append(arguments)
        return CommandResult(
            status: 0,
            stdout: "agentsview running at http://127.0.0.1:18080 (pid 42)\n",
        )
    }

    let url = try launcher.start()

    #expect(url?.absoluteString == "http://127.0.0.1:18080")
    #expect(calls == [ServerLauncher.startArguments])
}

@Test("uses the existing daemon URL when the background launcher is still starting")
func startFallsBackToStatus() throws {
    var calls: [[String]] = []
    let launcher = ServerLauncher(command: ["agentsview"]) { arguments in
        calls.append(arguments)
        if arguments == ServerLauncher.startArguments {
            return CommandResult(status: 0, stdout: "agentsview starting in background\n")
        }
        return CommandResult(
            status: 0,
            stdout: "agentsview running at http://127.0.0.1:18081\n",
        )
    }

    let url = try launcher.start()

    #expect(url?.absoluteString == "http://127.0.0.1:18081")
    #expect(calls == [ServerLauncher.startArguments, ServerLauncher.statusArguments])
}

@Test("honors an explicit agentsview binary")
func defaultCommandHonorsEnvironmentOverride() {
    #expect(
        ServerLauncher.defaultCommand(environment: ["AGENTSVIEW_BIN": "/tmp/agentsview"])
            == ["/tmp/agentsview"],
    )
}

@Test("stops the detached server through the existing lifecycle command")
func stopUsesServeStopCommand() throws {
    var calls: [[String]] = []
    let launcher = ServerLauncher(command: ["agentsview"]) { arguments in
        calls.append(arguments)
        return CommandResult(status: 0)
    }

    try launcher.stop()

    #expect(calls == [ServerLauncher.stopArguments])
}
