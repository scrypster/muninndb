// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "MuninnDB",
    platforms: [
        .iOS(.v15),
        .macOS(.v12),
        .tvOS(.v15),
        .watchOS(.v8),
    ],
    products: [
        .library(name: "MuninnDB", targets: ["MuninnDB"]),
    ],
    targets: [
        .target(
            name: "MuninnDB",
            path: "Sources/MuninnDB"
        ),
        .testTarget(
            name: "MuninnDBTests",
            dependencies: ["MuninnDB"],
            path: "Tests/MuninnDBTests"
        ),
    ]
)
