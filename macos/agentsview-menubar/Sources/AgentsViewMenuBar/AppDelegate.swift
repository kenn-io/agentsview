import AppKit
import Foundation

private struct URLResult: Sendable {
    let url: URL?
    let error: String?
}

private struct VoidResult: Sendable {
    let error: String?
}

@main
@MainActor
struct AgentsViewMenuBarMain {
    static func main() {
        let application = NSApplication.shared
        let delegate = AppDelegate()
        application.delegate = delegate
        application.setActivationPolicy(.accessory)
        application.run()
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let launcher = ServerLauncher()
    private var statusItem: NSStatusItem!
    private var serverURL: URL?
    private var statusMenuItem: NSMenuItem!

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        setupStatusItem()
        startServer()
    }

    private func setupStatusItem() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem.button?.title = "AV"
        statusItem.button?.toolTip = "AgentsView"

        let menu = NSMenu()
        statusMenuItem = menu.addItem(withTitle: "Starting AgentsView…", action: nil, keyEquivalent: "")
        statusMenuItem.isEnabled = false
        menu.addItem(.separator())
        menu.addItem(withTitle: "Open AgentsView", action: #selector(openAgentsView), keyEquivalent: "o")
        menu.addItem(withTitle: "Start / Refresh Server", action: #selector(startServer), keyEquivalent: "r")
        menu.addItem(withTitle: "Stop Server", action: #selector(stopServer), keyEquivalent: "s")
        menu.addItem(.separator())
        menu.addItem(withTitle: "Quit AgentsView Menu Bar", action: #selector(quit), keyEquivalent: "q")
        menu.items.forEach { $0.target = self }
        statusItem.menu = menu
    }

    @objc private func startServer() {
        updateStatus("Starting AgentsView…")
        let launcher = self.launcher
        Task { @MainActor [weak self] in
            let result = await Task.detached { () -> URLResult in
                do {
                    return URLResult(url: try launcher.start(), error: nil)
                } catch {
                    return URLResult(url: nil, error: error.localizedDescription)
                }
            }.value
            guard let self else { return }
            if let message = result.error {
                self.updateStatus("Server error: \(message)")
            } else {
                let url = result.url
                self.serverURL = url
                self.updateStatus(url.map { "Running at \($0.absoluteString)" } ?? "Running")
            }
        }
    }

    @objc private func openAgentsView() {
        if let serverURL {
            NSWorkspace.shared.open(serverURL)
            return
        }

        let launcher = self.launcher
        Task { @MainActor [weak self] in
            let url = await Task.detached {
                try? launcher.status()
            }.value ?? nil
            guard let self else { return }
            if let url {
                self.serverURL = url
                NSWorkspace.shared.open(url)
            } else {
                self.startServer()
            }
        }
    }

    @objc private func stopServer() {
        updateStatus("Stopping AgentsView…")
        let launcher = self.launcher
        Task { @MainActor [weak self] in
            let result = await Task.detached { () -> VoidResult in
                do {
                    try launcher.stop()
                    return VoidResult(error: nil)
                } catch {
                    return VoidResult(error: error.localizedDescription)
                }
            }.value
            guard let self else { return }
            if result.error == nil {
                self.serverURL = nil
                self.updateStatus("Server stopped")
            } else if let message = result.error {
                self.updateStatus("Stop error: \(message)")
            }
        }
    }

    @objc private func quit() {
        // The backend is intentionally detached. Users can keep AgentsView
        // serving after quitting the menu-bar controller and stop it explicitly.
        NSApp.terminate(nil)
    }

    private func updateStatus(_ title: String) {
        statusMenuItem?.title = title
    }
}
