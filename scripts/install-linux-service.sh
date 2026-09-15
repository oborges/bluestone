#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="bluestone"
SERVICE_USER="${SERVICE_USER:-bluestone}"
SERVICE_GROUP="${SERVICE_GROUP:-bluestone}"
INSTALL_BIN="${INSTALL_BIN:-/usr/local/bin/bluestone}"
CONFIG_DIR="${CONFIG_DIR:-/etc/bluestone}"
CONFIG_FILE="${CONFIG_FILE:-${CONFIG_DIR}/config.yaml}"
ENV_FILE="${ENV_FILE:-/etc/default/bluestone}"
SERVICE_FILE="${SERVICE_FILE:-/etc/systemd/system/${SERVICE_NAME}.service}"
CACHE_DIR="${CACHE_DIR:-/var/cache/bluestone}"
STAGING_DIR="${STAGING_DIR:-/var/staging/bluestone}"
LOG_DIR="${LOG_DIR:-/var/log/bluestone}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"

BUILD_MODE="auto"
BINARY_SOURCE=""
CONFIG_SOURCE=""
ENABLE_SERVICE=0
START_SERVICE=0
FORCE_CONFIG=0
FORCE_ENV=0
DRY_RUN=0

usage() {
	cat <<EOF
Usage: sudo ./scripts/install-linux-service.sh [options]

Installs Bluestone as a Linux systemd service.

Options:
  --build             Build ./bin/bluestone before installing.
  --no-build          Do not build; install ./bin/bluestone or --binary PATH.
  --binary PATH       Install an existing bluestone binary.
  --config PATH       Install this config as /etc/bluestone/config.yaml.
  --force-config      Replace an existing /etc/bluestone/config.yaml.
  --force-env         Replace an existing /etc/default/bluestone.
  --enable            Enable the service at boot.
  --start             Start or restart the service after installation.
  --dry-run           Print actions without changing the system.
  -h, --help          Show this help.

Environment overrides:
  SERVICE_USER, SERVICE_GROUP, INSTALL_BIN, CONFIG_DIR, CONFIG_FILE,
  ENV_FILE, SERVICE_FILE, CACHE_DIR, STAGING_DIR, LOG_DIR, VERSION
EOF
}

log() {
	printf '==> %s\n' "$*"
}

warn() {
	printf 'warning: %s\n' "$*" >&2
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

run() {
	if [[ "${DRY_RUN}" -eq 1 ]]; then
		printf '+'
		printf ' %q' "$@"
		printf '\n'
	else
		"$@"
	fi
}

while [[ "$#" -gt 0 ]]; do
	case "$1" in
		--build)
			BUILD_MODE="yes"
			;;
		--no-build)
			BUILD_MODE="no"
			;;
		--binary)
			[[ "$#" -ge 2 ]] || die "--binary requires a path"
			BINARY_SOURCE="$2"
			BUILD_MODE="no"
			shift
			;;
		--config)
			[[ "$#" -ge 2 ]] || die "--config requires a path"
			CONFIG_SOURCE="$2"
			shift
			;;
		--force-config)
			FORCE_CONFIG=1
			;;
		--force-env)
			FORCE_ENV=1
			;;
		--enable)
			ENABLE_SERVICE=1
			;;
		--start)
			START_SERVICE=1
			;;
		--dry-run)
			DRY_RUN=1
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			die "unknown option: $1"
			;;
	esac
	shift
done

need_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

check_host() {
	[[ "$(uname -s)" == "Linux" ]] || die "this installer must run on Linux"
	[[ "${EUID}" -eq 0 ]] || die "run this installer as root, for example: sudo $0"
	need_command install
	need_command mktemp
	need_command sed
	need_command systemctl
	need_command getent
	[[ -d /run/systemd/system ]] || die "systemd does not appear to be running on this host"
	if [[ -e /etc/systemd/system/nfs-gateway.service && "${SERVICE_NAME}" != "nfs-gateway" ]]; then
		die "found pre-rename /etc/systemd/system/nfs-gateway.service; installing ${SERVICE_NAME}.service alongside it would run two gateways on the same port and bucket. Migrate first: see docs/LINUX_SERVICE.md (Migrating From nfs-gateway)"
	fi
}

create_service_account() {
	local nologin="/usr/sbin/nologin"
	if [[ ! -x "${nologin}" ]]; then
		nologin="/sbin/nologin"
	fi
	if [[ ! -x "${nologin}" ]]; then
		nologin="/bin/false"
	fi

	if ! getent group "${SERVICE_GROUP}" >/dev/null; then
		log "Creating group ${SERVICE_GROUP}"
		run groupadd --system "${SERVICE_GROUP}"
	fi

	if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
		log "Creating user ${SERVICE_USER}"
		run useradd \
			--system \
			--gid "${SERVICE_GROUP}" \
			--home-dir /var/lib/bluestone \
			--no-create-home \
			--shell "${nologin}" \
			"${SERVICE_USER}"
	fi
}

resolve_version() {
	if [[ -n "${VERSION:-}" ]]; then
		printf '%s\n' "${VERSION}"
		return
	fi

	if command -v git >/dev/null 2>&1 && [[ -d "${REPO_ROOT}/.git" ]]; then
		git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || printf 'dev\n'
		return
	fi

	printf 'dev\n'
}

build_binary() {
	local default_binary="${REPO_ROOT}/bin/bluestone"
	local version

	if [[ -n "${BINARY_SOURCE}" ]]; then
		[[ -f "${BINARY_SOURCE}" ]] || die "binary not found: ${BINARY_SOURCE}"
		return
	fi

	if [[ "${BUILD_MODE}" == "yes" || ! -x "${default_binary}" ]]; then
		if [[ "${BUILD_MODE}" == "no" ]]; then
			die "binary not found: ${default_binary}; run make build, pass --build, or pass --binary PATH"
		fi

		need_command go
		version="$(resolve_version)"
		log "Building ${default_binary} with VERSION=${version}"
		run install -d -m 0755 "${REPO_ROOT}/bin"
		(
			cd "${REPO_ROOT}"
			run env CGO_ENABLED="${CGO_ENABLED:-0}" go build \
				-ldflags "-X main.Version=${version}" \
				-o "${default_binary}" \
				./cmd/bluestone
		)
	fi

	BINARY_SOURCE="${default_binary}"
}

sed_replacement_escape() {
	printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'
}

render_unit() {
	local unit_template="$1"
	local rendered_unit="$2"
	local user group config_file env_file writable_paths exec_start
	local cache_directory logs_directory

	user="$(sed_replacement_escape "${SERVICE_USER}")"
	group="$(sed_replacement_escape "${SERVICE_GROUP}")"
	config_file="$(sed_replacement_escape "${CONFIG_FILE}")"
	env_file="$(sed_replacement_escape "${ENV_FILE}")"
	writable_paths="$(sed_replacement_escape "${CACHE_DIR} ${STAGING_DIR} ${LOG_DIR}")"
	exec_start="$(sed_replacement_escape "${INSTALL_BIN} --config \${BLUESTONE_CONFIG}")"
	cache_directory="bluestone"
	logs_directory="bluestone"
	if [[ "${CACHE_DIR}" != "/var/cache/bluestone" ]]; then
		cache_directory=""
	fi
	if [[ "${LOG_DIR}" != "/var/log/bluestone" ]]; then
		logs_directory=""
	fi

	sed \
		-e "s|^User=.*|User=${user}|" \
		-e "s|^Group=.*|Group=${group}|" \
		-e "s|^Environment=BLUESTONE_CONFIG=.*|Environment=BLUESTONE_CONFIG=${config_file}|" \
		-e "s|^EnvironmentFile=.*|EnvironmentFile=-${env_file}|" \
		-e "s|^ExecStart=.*|ExecStart=${exec_start}|" \
		-e "s|^ReadWritePaths=.*|ReadWritePaths=${writable_paths}|" \
		-e "s|^CacheDirectory=.*|CacheDirectory=${cache_directory}|" \
		-e "s|^LogsDirectory=.*|LogsDirectory=${logs_directory}|" \
		"${unit_template}" > "${rendered_unit}"
}

install_files() {
	local config_template="${CONFIG_SOURCE:-${REPO_ROOT}/configs/config.example.yaml}"
	local env_template="${REPO_ROOT}/deployments/systemd/bluestone.env"
	local unit_template="${REPO_ROOT}/deployments/systemd/bluestone.service"
	local config_parent rendered_unit

	[[ -f "${BINARY_SOURCE}" ]] || die "binary not found: ${BINARY_SOURCE}"
	[[ -f "${config_template}" ]] || die "config template not found: ${config_template}"
	[[ -f "${env_template}" ]] || die "environment template not found: ${env_template}"
	[[ -f "${unit_template}" ]] || die "systemd unit template not found: ${unit_template}"

	log "Installing binary to ${INSTALL_BIN}"
	run install -d -m 0755 "$(dirname -- "${INSTALL_BIN}")"
	run install -m 0755 "${BINARY_SOURCE}" "${INSTALL_BIN}"

	log "Preparing runtime directories"
	run install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${CACHE_DIR}"
	run install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${STAGING_DIR}"
	run install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${LOG_DIR}"

	config_parent="$(dirname -- "${CONFIG_FILE}")"
	log "Installing configuration directory ${config_parent}"
	run install -d -m 0750 -o root -g "${SERVICE_GROUP}" "${config_parent}"
	if [[ "${FORCE_CONFIG}" -eq 1 || ! -e "${CONFIG_FILE}" ]]; then
		run install -m 0640 -o root -g "${SERVICE_GROUP}" "${config_template}" "${CONFIG_FILE}"
	else
		warn "keeping existing config ${CONFIG_FILE}; use --force-config to replace it"
	fi

	log "Installing environment file ${ENV_FILE}"
	run install -d -m 0755 "$(dirname -- "${ENV_FILE}")"
	if [[ "${FORCE_ENV}" -eq 1 || ! -e "${ENV_FILE}" ]]; then
		run install -m 0640 -o root -g "${SERVICE_GROUP}" "${env_template}" "${ENV_FILE}"
	else
		warn "keeping existing environment file ${ENV_FILE}; use --force-env to replace it"
	fi

	log "Installing systemd unit ${SERVICE_FILE}"
	run install -d -m 0755 "$(dirname -- "${SERVICE_FILE}")"
	rendered_unit="$(mktemp)"
	render_unit "${unit_template}" "${rendered_unit}"
	run install -m 0644 "${rendered_unit}" "${SERVICE_FILE}"
	rm -f "${rendered_unit}"
}

reload_and_optionally_start() {
	log "Reloading systemd"
	run systemctl daemon-reload

	if [[ "${ENABLE_SERVICE}" -eq 1 ]]; then
		log "Enabling ${SERVICE_NAME}.service"
		run systemctl enable "${SERVICE_NAME}.service"
	fi

	if [[ "${START_SERVICE}" -eq 1 ]]; then
		log "Starting ${SERVICE_NAME}.service"
		run systemctl restart "${SERVICE_NAME}.service"
	fi
}

print_summary() {
	cat <<EOF

Installed ${SERVICE_NAME}.

Next steps:
  1. Edit ${CONFIG_FILE} with the COS endpoint, bucket, region, and credentials.
  2. Review optional overrides in ${ENV_FILE}.
  3. Start it with: sudo systemctl enable --now ${SERVICE_NAME}
  4. Watch logs with: sudo journalctl -u ${SERVICE_NAME} -f

Installed paths:
  Binary:        ${INSTALL_BIN}
  Config:        ${CONFIG_FILE}
  Env overrides: ${ENV_FILE}
  Service unit:  ${SERVICE_FILE}
  Cache:         ${CACHE_DIR}
  Staging:       ${STAGING_DIR}
EOF
}

check_host
create_service_account
build_binary
install_files
reload_and_optionally_start
print_summary
