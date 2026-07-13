import Foundation

public struct CommandResult: Equatable {
    public let status: Int32
    public let stdout: String
    public let stderr: String

    public init(status: Int32, stdout: String = "", stderr: String = "") {
        self.status = status
        self.stdout = stdout
        self.stderr = stderr
    }
}

public enum ServerLauncherError: Error, Equatable, CustomStringConvertible {
    case commandFailed(arguments: [String], status: Int32, output: String)

    public var description: String {
        switch self {
        case let .commandFailed(arguments, status, output):
            let detail = output.isEmpty ? "" : ": \(output)"
            return "agentsview \(arguments.joined(separator: " ")) exited with status \(status)\(detail)"
        }
    }
}

public typealias CommandRunner = ([String]) throws -> CommandResult

public final class ServerLauncher: @unchecked Sendable {
    public static let startArguments = [
        "serve", "--background", "--no-browser", "--port", "0",
    ]
    public static let statusArguments = ["serve", "status"]
    public static let stopArguments = ["serve", "stop"]

    private let command: [String]
    private let runner: CommandRunner

    public init(
        command: [String] = ServerLauncher.defaultCommand(),
        runner: CommandRunner? = nil,
    ) {
        self.command = command
        self.runner = runner ?? { arguments in
            try Self.runProcess(command: command, arguments: arguments)
        }
    }

    @discardableResult
    public func start() throws -> URL? {
        let result = try run(Self.startArguments)
        return try serverURL(from: result) ?? status()
    }

    public func status() throws -> URL? {
        let result = try run(Self.statusArguments)
        return serverURL(from: result)
    }

    public func stop() throws {
        _ = try run(Self.stopArguments)
    }

    public static func defaultCommand(
        environment: [String: String] = ProcessInfo.processInfo.environment,
    ) -> [String] {
        if let configured = environment["AGENTSVIEW_BIN"], !configured.isEmpty {
            return [configured]
        }

        for path in [
            "/opt/homebrew/bin/agentsview",
            "/usr/local/bin/agentsview",
            (environment["HOME"] ?? "") + "/.local/bin/agentsview",
        ] where !path.isEmpty && FileManager.default.isExecutableFile(atPath: path) {
            return [path]
        }

        // Keep PATH lookup working for development installs and package
        // managers that place the binary somewhere else.
        return ["/usr/bin/env", "agentsview"]
    }

    private func run(_ arguments: [String]) throws -> CommandResult {
        let result = try runner(arguments)
        guard result.status == 0 else {
            throw ServerLauncherError.commandFailed(
                arguments: arguments,
                status: result.status,
                output: combinedOutput(result),
            )
        }
        return result
    }

    private func serverURL(from result: CommandResult) -> URL? {
        let output = combinedOutput(result)
        for marker in [
            "agentsview running at ",
            "agentsview already running at ",
        ] {
            guard let range = output.range(of: marker) else { continue }
            let remainder = output[range.upperBound...]
            guard let token = remainder.split(whereSeparator: { $0.isWhitespace }).first,
                  let url = URL(string: String(token))
            else { continue }
            return url
        }
        return nil
    }

    private func combinedOutput(_ result: CommandResult) -> String {
        [result.stdout, result.stderr]
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
            .joined(separator: "\n")
    }

    private static func runProcess(command: [String], arguments: [String]) throws -> CommandResult {
        precondition(!command.isEmpty)

        let process = Process()
        process.executableURL = URL(fileURLWithPath: command[0])
        process.arguments = Array(command.dropFirst()) + arguments

        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr
        try process.run()
        process.waitUntilExit()

        return CommandResult(
            status: process.terminationStatus,
            stdout: String(data: stdout.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? "",
            stderr: String(data: stderr.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? "",
        )
    }
}
