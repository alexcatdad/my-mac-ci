// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "MMC",
    platforms: [.macOS(.v13)],
    products: [.executable(name: "MMC", targets: ["MMC"])],
    dependencies: [
        // MenubarKit v2026.11.07.2.
        .package(
            url: "https://github.com/alexcatdad/menubar-kit.git",
            revision: "cf3666e8e1a822930c7f4c3f64309de5951b1c54")
    ],
    targets: [
        .target(name: "MMCLoginKeychain", linkerSettings: [.linkedFramework("Security")]),
        .executableTarget(
            name: "MMC",
            dependencies: ["MMCLoginKeychain", .product(name: "MenubarKit", package: "menubar-kit")]),
        .testTarget(name: "MMCTests", dependencies: ["MMC"]),
    ]
)
