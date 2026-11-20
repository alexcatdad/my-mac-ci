#!/bin/sh
# Build or validate the MMC arm64 release artifact inside reltool's checkout.
set -eu
: "${RELEASE_OUT:?}" "${RELEASE_VERSION:?}" "${MACOS_SHORT_VERSION:?}" "${MACOS_BUNDLE_VERSION:?}"
archive="$RELEASE_OUT/my-mac-ci-$RELEASE_VERSION-darwin-arm64.zip"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
app="$stage/MMC.app"
contents="$app/Contents"
daemon=com.alexcatdad.my-mac-ci.pf
plist="$contents/Info.plist"
case "$1" in
build)
    : "${MACOS_SIGNING_IDENTITY:?Developer ID required}"
    mkdir -p "$contents/MacOS" "$contents/Helpers" "$contents/Resources" "$contents/Library/LaunchDaemons" "$RELEASE_OUT"
    swift build -c release --arch arm64 --package-path macos --product MMC -Xswiftc -warnings-as-errors
    bin=$(swift build -c release --arch arm64 --package-path macos --show-bin-path)
    cp "$bin/MMC" "$contents/MacOS/MMC"
    cp macos/Resources/Info.plist "$plist"
    cp macos/Resources/AppIcon.icns macos/Resources/MMCTrayIcon*.png "$contents/Resources/"
    /usr/libexec/PlistBuddy -c 'Set :CFBundleIdentifier com.alexcatdad.my-mac-ci' \
        -c 'Set :CFBundleName MMC' -c 'Set :CFBundleDisplayName MMC' \
        -c "Set :CFBundleShortVersionString $MACOS_SHORT_VERSION" -c "Set :CFBundleVersion $MACOS_BUNDLE_VERSION" \
        -c "Add :ReleaseVersion string $RELEASE_VERSION" "$plist"
    cp "macos/Resources/LaunchDaemons/$daemon.plist" "$contents/Library/LaunchDaemons/"
    for tool in mmc-helper mmc mmc-pf; do
        MACOSX_DEPLOYMENT_TARGET=13.0 CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -o "$contents/Helpers/$tool" "./cmd/$tool"
        codesign --force --options runtime --timestamp --identifier "com.alexcatdad.my-mac-ci.$tool" --sign "$MACOS_SIGNING_IDENTITY" "$contents/Helpers/$tool"
    done
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$contents/Resources/mmc-probe" ./cmd/mmc-probe
    codesign --force --options runtime --timestamp --sign "$MACOS_SIGNING_IDENTITY" "$app"
    ditto -c -k --sequesterRsrc --keepParent "$app" "$archive"
    ;;
validate)
    ditto -x -k "$archive" "$stage"
    codesign --verify --strict --deep -R '=anchor apple generic and certificate leaf[field.1.2.840.113635.100.6.1.13] exists' "$app"
    test "$(plutil -extract CFBundleIdentifier raw "$plist")" = com.alexcatdad.my-mac-ci
    test "$(plutil -extract ReleaseVersion raw "$plist")" = "$RELEASE_VERSION"
    test "$(plutil -extract CFBundleVersion raw "$plist")" = "$MACOS_BUNDLE_VERSION"
    test "$(plutil -extract CFBundleIconFile raw "$plist")" = AppIcon
    for resource in AppIcon.icns MMCTrayIcon.png MMCTrayIcon@2x.png; do
        test -s "$contents/Resources/$resource"
    done
    path=$(plutil -extract BundleProgram raw "$contents/Library/LaunchDaemons/$daemon.plist")
    test "$path" = Contents/Helpers/mmc-pf
    for tool in "$contents/MacOS/MMC" "$contents/Helpers/mmc" "$contents/Helpers/mmc-helper" "$app/$path"; do
        test "$(lipo -archs "$tool")" = arm64
        codesign --verify --strict "$tool"
    done
    file "$contents/Resources/mmc-probe" | grep -q 'ELF 64-bit.*ARM aarch64'
    "$contents/Helpers/mmc-helper" check-config -h >/dev/null
    ;;
*) exit 2 ;;
esac
