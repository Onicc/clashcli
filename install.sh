#!/bin/sh
# Install the official release CLI only; initialization is an explicit next step.
# Keep all execution inside main so a truncated curl | sh cannot partially run it.
set -eu

main() {
    version=latest
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --version)
                [ "$#" -ge 2 ] || die '--version requires a tag, for example v0.1.1'
                version=$2
                shift 2
                ;;
            -h|--help)
                printf '%s\n' 'Usage: sh install.sh [--version vX.Y.Z]' \
                    'Install/upgrade clashcli in /usr/local/bin. Default: latest stable release.' \
                    'Requires Linux amd64/arm64, curl, coreutils, and root or sudo.' \
                    'No subscriptions, services, proxy settings, or core binaries are changed.'
                return
                ;;
            *) die "Unknown option: $1" ;;
        esac
    done
    [ "$(uname -s)" = Linux ] || die 'Only Linux is supported.'
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die 'Only amd64 and arm64 are supported.' ;;
    esac
    for command in curl sha256sum awk grep mktemp install mv rm id; do
        command -v "$command" >/dev/null 2>&1 || die "Missing dependency: $command"
    done
    if [ "$(id -u)" != 0 ]; then
        command -v sudo >/dev/null 2>&1 || die 'Run as root or install sudo.'
    fi
    repo=https://github.com/Onicc/clashcli
    if [ "$version" = latest ]; then
        # Resolve once; both assets must come from the exact same release.
        release_url=$(fetch --head --output /dev/null --write-out '%{url_effective}' "$repo/releases/latest") || die 'Cannot resolve latest release; try --version vX.Y.Z.'
        case "$release_url" in
            "$repo/releases/tag/"*) version=${release_url##*/} ;;
            *) die 'Unexpected release redirect.' ;;
        esac
    fi
    printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$' || die 'Invalid version tag; expected vX.Y.Z.'
    asset=clashcli-linux-$arch
    umask 077
    download_dir=$(mktemp -d /tmp/clashcli-install.XXXXXXXXXX)
    trap 'rm -rf -- "$download_dir"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP
    printf 'Downloading clashcli %s (%s)…\n' "$version" "$arch"
    fetch --max-filesize 67108864 --output "$download_dir/$asset" "$repo/releases/download/$version/$asset"
    fetch --max-filesize 65536 --output "$download_dir/SHA256SUMS" "$repo/releases/download/$version/SHA256SUMS"
    # Never pass an untrusted manifest directly to sha256sum -c: it may name
    # arbitrary local files. Extract exactly one well-formed, expected entry.
    digest=$(awk -v name="$asset" '$2 == name && NF == 2 { print $1; count++ } END { if (count != 1) exit 1 }' "$download_dir/SHA256SUMS") || die 'Missing or duplicate checksum entry.'
    printf '%s\n' "$digest" | grep -Eq '^[a-fA-F0-9]{64}$' || die 'Invalid SHA-256 checksum.'
    (cd "$download_dir" && printf '%s  %s\n' "$digest" "$asset" | sha256sum -c -) || die 'SHA-256 verification failed; existing installation unchanged.'
    # The privileged section is deliberately small. Stage on the destination
    # filesystem, then rename atomically (also works while the old CLI runs).
    # Variables in this literal script must expand in the privileged child only.
    # shellcheck disable=SC2016
    install_command='
        set -eu
        PATH=/usr/sbin:/usr/bin:/sbin:/bin
        export PATH
        target=/usr/local/bin/clashcli
        if [ -L "$target" ] || { [ -e "$target" ] && [ ! -f "$target" ]; }; then
            printf "%s\n" "Refusing to replace a non-regular file: $target" >&2
            exit 1
        fi
        if [ ! -d /usr/local/bin ]; then install -d -m 0755 /usr/local/bin; fi
        candidate=$(mktemp /usr/local/bin/.clashcli-install.XXXXXXXXXX)
        trap '\''rm -f -- "$candidate"'\'' EXIT
        trap '\''exit 130'\'' INT
        trap '\''exit 143'\'' TERM
        trap '\''exit 129'\'' HUP
        install -m 0755 -- "$1" "$candidate"
        # Recheck the root-owned copy to close the download-to-sudo race.
        printf "%s  %s\n" "$2" "$candidate" | sha256sum -c -
        actual_version=$("$candidate" --version)
        [ "$actual_version" = "clashcli version $3" ] || {
            printf "%s\n" "Binary cannot run or version does not match release tag." >&2
            exit 1
        }
        mv -fT -- "$candidate" "$target"
    '
    if [ "$(id -u)" = 0 ]; then
        sh -c "$install_command" sh "$download_dir/$asset" "$digest" "$version"
    else
        sudo sh -c "$install_command" sh "$download_dir/$asset" "$digest" "$version"
    fi
    printf '%s\n' "Installed clashcli $version at /usr/local/bin/clashcli" \
        '首次使用：clashcli init' \
        '升级已保留配置和运行状态；输入 clashcli 打开菜单。'
}

die() { printf 'clashcli installer: %s\n' "$*" >&2; exit 1; }
fetch() {
    # Disable ~/.curlrc, require HTTPS even across redirects, and bound retries.
    curl -q --fail --silent --show-error --location --proto '=https' \
        --proto-redir '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
        --retry 3 --retry-max-time 240 "$@"
}

main "$@"
