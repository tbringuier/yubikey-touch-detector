#!/usr/bin/env bash
# Install script for Aurora / Bazzite (Fedora Atomic and derivatives)
# Usage: ./install.sh [--uninstall]
set -euo pipefail

BINARY_NAME="yubikey-touch-detector"
INSTALL_DIR="${HOME}/.local/bin"
SYSTEMD_DIR="${HOME}/.config/systemd/user"
CONFIG_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/${BINARY_NAME}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOLBOX_NAME="${BINARY_NAME}-build"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }
heading() { echo -e "\n${BOLD}==> $*${NC}"; }

is_gnome() {
    [[ "${XDG_CURRENT_DESKTOP:-}" == *"GNOME"* ]] || \
    [[ "${DESKTOP_SESSION:-}" == *"gnome"* ]]
}

# ── Uninstall ────────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--uninstall" ]]; then
    heading "Uninstalling ${BINARY_NAME}"
    systemctl --user disable --now "${BINARY_NAME}.service" 2>/dev/null || true
    systemctl --user disable --now "${BINARY_NAME}.socket"  2>/dev/null || true
    rm -f "${SYSTEMD_DIR}/${BINARY_NAME}.service"
    rm -f "${SYSTEMD_DIR}/${BINARY_NAME}.socket"
    systemctl --user daemon-reload
    rm -f "${INSTALL_DIR}/${BINARY_NAME}"
    info "Uninstalled. Config kept in ${CONFIG_DIR} — remove manually if desired."
    exit 0
fi

# ── Sanity checks ────────────────────────────────────────────────────────────
heading "Pre-flight checks"

if [[ "${EUID}" -eq 0 ]]; then
    error "Do not run this script as root."
fi

if ! command -v toolbox &>/dev/null; then
    error "toolbox is required but not found.
On Fedora Atomic systems (Aurora, Bazzite, Silverblue…) toolbox is pre-installed.
If it is missing, install it with: rpm-ostree install toolbox"
fi

mkdir -p "${INSTALL_DIR}" "${SYSTEMD_DIR}" "${CONFIG_DIR}"

# ── GNOME: install AppIndicator extension ────────────────────────────────────
if is_gnome; then
    heading "GNOME detected — installing AppIndicator extension"
    echo ""
    echo "  The tray icon requires the AppIndicator GNOME Shell extension."
    echo "  This will layer the package onto your system image via rpm-ostree."
    echo "  A reboot will be needed afterwards to activate it."
    echo ""
    read -rp "  Install gnome-shell-extension-appindicator via rpm-ostree? [Y/n] " yn
    case "${yn,,}" in
        n|no)
            warn "Skipped. The tray icon will not be visible on GNOME without this extension."
            warn "Install later with: rpm-ostree install gnome-shell-extension-appindicator"
            ;;
        *)
            info "Running: rpm-ostree install gnome-shell-extension-appindicator"
            rpm-ostree install --idempotent gnome-shell-extension-appindicator
            echo ""
            warn "Package staged. You must REBOOT before the extension becomes active."
            warn "After reboot, open 'Extensions' or GNOME Tweaks and enable AppIndicator."
            echo ""
            read -rp "  Continue installation without rebooting now? [Y/n] " cont
            [[ "${cont,,}" == n* ]] && exit 0
            ;;
    esac
fi

# ── Build inside toolbox ─────────────────────────────────────────────────────
heading "Building ${BINARY_NAME} inside toolbox '${TOOLBOX_NAME}'"

if ! toolbox list --containers 2>/dev/null | grep -qw "${TOOLBOX_NAME}"; then
    info "Creating toolbox container '${TOOLBOX_NAME}'..."
    toolbox create --container "${TOOLBOX_NAME}"
fi

info "Installing build dependencies and compiling..."
toolbox run --container "${TOOLBOX_NAME}" bash -c "
    set -e
    sudo dnf install -y golang gpgme-devel git 2>/dev/null
    cd '${REPO_DIR}'
    go build -o '/tmp/${BINARY_NAME}' .
    echo 'Build successful.'
"

BUILD_BINARY="/tmp/${BINARY_NAME}"

# ── Detect pre-existing state before touching anything ───────────────────────
# Remember whether the service was running and whether the binary changed,
# so we can decide between a fresh start and a hot restart at the end.
was_running=false
if systemctl --user is-active --quiet "${BINARY_NAME}.service" 2>/dev/null; then
    was_running=true
fi

old_checksum=""
if [[ -f "${INSTALL_DIR}/${BINARY_NAME}" ]]; then
    old_checksum=$(sha256sum "${INSTALL_DIR}/${BINARY_NAME}" | cut -d' ' -f1)
fi

# ── Install binary ───────────────────────────────────────────────────────────
heading "Installing binary"
install -Dm755 "${BUILD_BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"

new_checksum=$(sha256sum "${INSTALL_DIR}/${BINARY_NAME}" | cut -d' ' -f1)
binary_updated=false
if [[ "${new_checksum}" != "${old_checksum}" ]]; then
    binary_updated=true
    info "Binary installed/updated at ${INSTALL_DIR}/${BINARY_NAME}"
else
    info "Binary at ${INSTALL_DIR}/${BINARY_NAME} is already up to date (same checksum)."
fi

if [[ ":${PATH}:" != *":${INSTALL_DIR}:"* ]]; then
    warn "${INSTALL_DIR} is not in your PATH."
    warn "Add to your ~/.bashrc or ~/.profile:"
    warn '  export PATH="$HOME/.local/bin:$PATH"'
fi

# ── Systemd units ────────────────────────────────────────────────────────────
heading "Installing systemd user units"

cat > "${SYSTEMD_DIR}/${BINARY_NAME}.service" <<EOF
[Unit]
Description=Detects when your YubiKey is waiting for a touch
Requires=${BINARY_NAME}.socket

[Service]
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
EnvironmentFile=-%E/${BINARY_NAME}/service.conf

[Install]
Also=${BINARY_NAME}.socket
WantedBy=default.target
EOF

cat > "${SYSTEMD_DIR}/${BINARY_NAME}.socket" <<EOF
[Unit]
Description=Unix socket activation for YubiKey touch detector service

[Socket]
ListenStream=%t/${BINARY_NAME}.socket
RemoveOnStop=yes

[Install]
WantedBy=sockets.target
EOF

info "Systemd units written to ${SYSTEMD_DIR}/"

# ── Configuration ────────────────────────────────────────────────────────────
heading "Writing default configuration"

if [[ ! -f "${CONFIG_DIR}/service.conf" ]]; then
    cp "${REPO_DIR}/service.conf.example" "${CONFIG_DIR}/service.conf"
    info "Default config written to ${CONFIG_DIR}/service.conf"
else
    info "Config already exists at ${CONFIG_DIR}/service.conf — skipping."
fi

# ── Enable / restart service ─────────────────────────────────────────────────
heading "Starting systemd service"

systemctl --user daemon-reload

if [[ "${was_running}" == true ]]; then
    if [[ "${binary_updated}" == true ]]; then
        info "Service was running and binary changed — restarting..."
        systemctl --user restart "${BINARY_NAME}.service"
    else
        info "Service already running and binary unchanged — no restart needed."
    fi
else
    # Fresh install or service was stopped: enable and start.
    systemctl --user enable --now "${BINARY_NAME}.service"
fi

systemctl --user status "${BINARY_NAME}.service" --no-pager || true

# ── Done ─────────────────────────────────────────────────────────────────────
echo ""
info "Installation complete!"
echo -e "
  Binary:  ${INSTALL_DIR}/${BINARY_NAME}
  Config:  ${CONFIG_DIR}/service.conf
  Service: systemctl --user status ${BINARY_NAME}.service

  To test notifications, run:
    ${BINARY_NAME} --libnotify --tray -v

  To view logs:
    journalctl --user -u ${BINARY_NAME}.service -f
"

if is_gnome; then
    echo -e "${YELLOW}GNOME reminder:${NC} reboot and enable the AppIndicator extension"
    echo "  to make the tray icon visible."
    echo ""
fi
