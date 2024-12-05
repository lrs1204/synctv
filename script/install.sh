#!/bin/bash

download_tools_list=(
    "curl"
    "wget"
)

function Help() {
    echo "Usage: sudo -v ; curl -fsSL https://raw.githubusercontent.com/synctv-org/synctv/main/script/install.sh | sudo bash -s -- -v latest"
    echo "-h: help"
    echo "-v: install version (default: latest)"
    echo "-p: github proxy (default: https://mirror.ghproxy.com/)"
    echo "-m: micro architecture (no default value)"
    echo "  example: -m v2"
    echo "  example: -m 6"
}

function Init() {
    # Check if the user is root or sudo
    if [ "$EUID" -ne 0 ]; then
        echo "Please run as root"
        exit
    fi
    VERSION="latest"
    GH_PROXY="https://mirror.ghproxy.com/"
    InitOS
    InitArch
    InitDownloadTools
}

function ParseArgs() {
    while getopts "hv:p:m:" arg; do
        case $arg in
        h)
            Help
            exit 0
            ;;
        v)
            VERSION="$OPTARG"
            ;;
        p)
            GH_PROXY="$OPTARG"
            ;;
        m)
            Microarchitecture="$OPTARG"
            ;;
        ?)
            echo "unkonw argument"
            exit 1
            ;;
        esac
    done
}

function FixArgs() {
    # 如果GH_PROXY结尾不是/，则补上
    if [ "${GH_PROXY: -1}" != "/" ]; then
        GH_PROXY="$GH_PROXY/"
    fi
    # 如果VERSION不是以v开头且不是latest、dev，则补上v
    if [[ "$VERSION" != v* ]] && [ "$VERSION" != "latest" ] && [ "$VERSION" != "dev" ]; then
        VERSION="v$VERSION"
    fi

}

function InitOS() {
    case "$(uname)" in
    Linux)
        OS='linux'
        ;;
    # Darwin)
    #     OS='darwin'
    #     ;;
    *)
        echo "OS: ${OS} not supported"
        exit 2
        ;;
    esac
}

# Ref: https://dl.xanmod.org/check_x86-64_psabi.sh
# https://go.dev/wiki/MinimumRequirements#amd64
AMD64_MICRO_DETECTION_SCRIPT=$(
    cat <<EOF
BEGIN {
    while (!/flags/) if (getline < "/proc/cpuinfo" != 1) exit 1
    if (/lm/&&/cmov/&&/cx8/&&/fpu/&&/fxsr/&&/mmx/&&/syscall/&&/sse2/) level = 1
    if (level == 1 && /cx16/&&/lahf/&&/popcnt/&&/sse4_1/&&/sse4_2/&&/ssse3/) level = 2
    if (level == 2 && /avx/&&/avx2/&&/bmi1/&&/bmi2/&&/f16c/&&/fma/&&/abm/&&/movbe/&&/xsave/) level = 3
    if (level == 3 && /avx512f/&&/avx512bw/&&/avx512cd/&&/avx512dq/&&/avx512vl/) level = 4
    if (level > 0) { print "v" level; exit 0 }
    exit 1
}
EOF
)

function InitArch() {
    case "$(uname -m)" in
    x86_64 | amd64)
        ARCH='amd64'
        if [ ! "$Microarchitecture" ]; then
            Microarchitecture="$(awk "$AMD64_MICRO_DETECTION_SCRIPT")"
        fi
        ;;
    i?86 | x86)
        ARCH='386'
        ;;
    arm64 | aarch64)
        ARCH='arm64'
        ;;
    arm*)
        ARCH='arm'
        ;;
    *)
        echo "arch: ${ARCH} not supported"
        exit 2
        ;;
    esac
}

function CurrentVersion() {
    if [ -n "$(command -v synctv)" ]; then
        echo "$(synctv version | head -n 1 | awk '{print $2}')"
    else
        echo "uninstalled"
    fi
}

function InitDownloadTools() {
    for tool in "${download_tools_list[@]}"; do
        if [ -n "$(command -v $tool)" ]; then
            download_tool="$tool"
            break
        fi
    done
    if [ -z "$download_tool" ]; then
        echo "no download tools"
        exit 1
    fi
}

function Download() {
    case "$download_tool" in
    curl)
        status_code=$(curl -L "$1" -o "$2" --progress-bar -w "%{http_code}\n")
        if [ $? -ne 0 ]; then
            echo "download $1 failed"
            exit 1
        fi
        if [ "$status_code" != "200" ]; then
            echo "download $1 failed, status code: $status_code"
            exit 1
        fi
        ;;
    wget)
        wget -O "$2" "$1"
        if [ $? -ne 0 ]; then
            echo "download $1 failed"
            exit 1
        fi
        ;;
    *)
        echo "download tool: $download_tool not supported"
        exit 1
        ;;
    esac
}

function DownloadURL() {
    if [ -n "$Microarchitecture" ] && [ "${Microarchitecture:0:1}" != "-" ]; then
        Microarchitecture="-$Microarchitecture"
    fi
    if [[ $1 == v* ]]; then
        echo "${GH_PROXY}https://github.com/lrs1204/synctv/releases/download/$1/synctv-${OS}-${ARCH}${Microarchitecture}"
    else
        echo "${GH_PROXY}https://github.com/lrs1204/synctv/releases/$1/download/synctv-${OS}-${ARCH}${Microarchitecture}"
    fi
}

function InstallWithVersion() {
    tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t 'synctv-install.XXXXXXXXXX')
    trap 'rm -rf "$tmp_dir"' EXIT

    URL="$(DownloadURL "$1")"
    echo "download: $URL"

    case "$OS" in
    linux)
        Download "$URL" "$tmp_dir/synctv"

        cp "$tmp_dir/synctv" /usr/bin/synctv.new
        if [ $? -ne 0 ]; then
            echo "copy synctv to /usr/bin/synctv.new failed"
            exit 1
        fi

        chmod 755 /usr/bin/synctv.new
        chown root:root /usr/bin/synctv.new
        mv /usr/bin/synctv{.new,}
        if [ $? -ne 0 ]; then
            echo "move /usr/bin/synctv{.new,} failed"
            exit 1
        fi
        echo "synctv installed to /usr/bin/synctv"
        ;;
    darwin)
        Download "$URL" "$tmp_dir/synctv"

        mkdir -m 0555 -p /usr/local/bin
        if [ $? -ne 0 ]; then
            echo "mkdir /usr/local/bin failed"
            exit 1
        fi

        cp "$tmp_dir/synctv" /usr/local/bin/synctv.new
        if [ $? -ne 0 ]; then
            echo "copy synctv to /usr/local/bin/synctv.new failed"
            exit 1
        fi

        chmod a=x /usr/local/bin/synctv.new
        mv /usr/local/bin/synctv{.new,}
        if [ $? -ne 0 ]; then
            echo "move /usr/local/bin/synctv{.new,} failed"
            exit 1
        fi
        echo "synctv installed to /usr/local/bin/synctv"
        ;;
    *)
        echo 'OS not supported'
        exit 2
        ;;
    esac
}

function InitLinuxSystemctlService() {
    if [ -z "$(command -v systemctl)" ]; then
        echo "systemctl command not found"
        exit 1
    fi
    mkdir -p "/opt/synctv"
    if [ ! -d "/etc/systemd/system" ]; then
        echo "/etc/systemd/system not found"
        exit 1
    fi

    if [ -f "/etc/systemd/system/synctv.service" ]; then
        return
    fi

    if [ -f "./script/synctv.service" ]; then
        echo "use ./script/synctv.service"
        cp "./script/synctv.service" "/etc/systemd/system/synctv.service"
        if [ $? -ne 0 ]; then
            echo "copy ./script/synctv.service to /etc/systemd/system/synctv.service failed"
            exit 1
        fi
    else
        echo "use default synctv.service"
        cat <<EOF >"/etc/systemd/system/synctv.service"
[Unit]
Description=SyncTV Service
After=network.target

[Service]
ExecStart=/usr/bin/synctv server --data-dir /opt/synctv
WorkingDirectory=/opt/synctv
Restart=unless-stopped

[Install]
WantedBy=multi-user.target
EOF
        if [ $? -ne 0 ]; then
            echo "write /etc/systemd/system/synctv.service failed"
            exit 1
        fi
    fi

    systemctl daemon-reload
    echo "/etc/systemd/system/synctv.service install success"
    echo "run: systemctl enable synctv && systemctl start synctv"
}

function InitSystemctlService() {
    case "$OS" in
    linux)
        InitLinuxSystemctlService
        ;;
    esac
}

function Install() {
    current_version="$(CurrentVersion)"
    echo "current version: $current_version"
    echo "install version: $VERSION"
    if [ "$current_version" != "uninstalled" ] && [ "$current_version" = "$VERSION" ] && [ "$current_version" != "dev" ]; then
        echo "current version is $current_version, skip"
        exit 0
    fi

    InstallWithVersion "$VERSION"

    echo "install success"
}

Init
ParseArgs "$@"
FixArgs
Install
InitSystemctlService
