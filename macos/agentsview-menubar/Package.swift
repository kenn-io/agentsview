// swift-tools-version: 6.0

import PackageDescription

let package = Package(
    name: "AgentsViewMenuBar",
    platforms: [.macOS(.v11)],
    products: [
        .executable(name: "AgentsViewMenuBar", targets: ["AgentsViewMenuBar"]),
    ],
    targets: [
        .executableTarget(name: "AgentsViewMenuBar"),
        .testTarget(
            name: "AgentsViewMenuBarTests",
            dependencies: ["AgentsViewMenuBar"],
        ),
    ],
)
