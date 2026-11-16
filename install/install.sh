#!/bin/sh
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Installs Device Connect for AWS Device Farm (devicefarm-connect) on macOS or Linux:
#
#   curl -fsSL https://github.com/aws/device-connect-devicefarm/releases/latest/download/install.sh | sh
#
# Installs only the release that this file is part of.
#
# Environment:
#   DEVICEFARM_CONNECT_INSTALL_DIR       Install directory. Default: $HOME/.local/bin
#   DEVICEFARM_CONNECT_INSTALL_BASE_URL  Download source, for a mirror. Default: the GitHub releases
#
# Checks, in this order. A failed check stops the install:
#   1. OpenPGP signature of SHA256SUMS. Skipped with a notice when gpg or gpgv
#      is missing, or when this file holds no release key.
#   2. SHA-256 checksum of the program against SHA256SUMS.
#   3. macOS only: Apple signature of the release team, and notarization (codesign).
#
# Needs no root. Changes no shell profile. Never runs the program.

set -eu

readonly VERSION='v1.0.0'
# Apple Developer team that signs the release.
readonly APPLE_TEAM_ID='94KV3E626L'
readonly PROGRAM_NAME='devicefarm-connect'
readonly RELEASES_URL='https://github.com/aws/device-connect-devicefarm/releases'
readonly DEFAULT_BASE_URL="$RELEASES_URL/download"
# Release file: SHA-256 checksums of the release files.
readonly CHECKSUM_LIST='SHA256SUMS'

# EXIT trap removes these.
work_dir=''
staged_program=''

# Armored release public key. Empty: no signature check.
print_release_key() {
cat <<'RELEASE_KEY'
-----BEGIN PGP PUBLIC KEY BLOCK-----
Comment: 492C 96C0 8389 3C96 0BA4  86DE 236A E7FF FA0D 086E
Comment: Device Connect for AWS Device Farm <aws-devicefarm-supp

xsFNBGr8hMwBEACQTMhDZ0tuEkysoiZZXcCquLJKlbuVP4rUFKSi4qofc3TlMk2g
Q1DjMsSknZhBg+oYFxXWvhbr43WK8TL7sv9LlEAUR4jZzKQqpmCLqMc6uxEX6tuX
tuob3N6iEe6ODo1WG5XTvJCuHF6w+2al8pVVpJ+rkPHchTTqr2DM3z4SBBTPzPNG
5YLYtOx4+Ezah73Rk8eDF19WZYa0V6ROlPMkMW1As6mSlXJZ0zdueiICwBee7k1Q
wzdFfbGMDws25sMAuXEJDDBKmAsjCUK8l9okBkZryCuJ5y8bApEwWHNNtqCHIZr2
nfLCqu8nvTPpQRa8ls9QF0SULHqeDYuQ3SXZVRnwp0q2LvneV0yz4llEQhhoIMc4
iACJl8lbYzQT5TECbebtV3BaxxPmtQPxbr7pYIwqJJbsuAVuDr6OiggUXI2pNykf
RY5Px+dKdaxlZUeZhR7Ws5FZF52McMLwZQbqcOaGb1VyQpT7VgnSrvUUVxRZowOj
VKgGJbir4eOtdil0azRtvA15gO5jkFRy0Yx9WpD1h9pwnUxfUR3ywpnCroeiWZ8o
8fHozYyHm/4t8pcwH6Tzuhc9ggBdzdGVyn1Jp4vnMWPfspMJqTR6COOyeosCU+8d
vpxc8xtNdbZswirqfkz+pq/h3EqL/c72N8oKRsPJkXjOJi4dkfWbOVgQ8QARAQAB
zUZEZXZpY2UgQ29ubmVjdCBmb3IgQVdTIERldmljZSBGYXJtIDxhd3MtZGV2aWNl
ZmFybS1zdXBwb3J0QGFtYXpvbi5jb20+wsG+BBMBCgByBYJq/lh5CRAjauf/+g0I
bkcUAAAAAAAeACBzYWx0QG5vdGF0aW9ucy5zZXF1b2lhLXBncC5vcmelZsyeD18R
C6+TbEYMAOqoWPoQosEmqdc8WWm3sTPW4QKbAxYhBEkslsCDiTyWC6SG3iNq5//6
DQhuAAD/cw/+MTRDSXrUfRl7k2LOcHuezQ8wOLg96+Yr2y/uTlELpsvrRAxmT/Il
Y8ma6h9mgy5Xk+VlbrOY9FPD5HN4a3j0IHrCJFX0ruUyUgus6lf+GejNwkXh2/Z/
qwDJDPcOAHLm6f/jvcFxK8DnTEG4ZfkWgXH7Epqok4nAzmdrvDHTnCLDsQj/4gi7
RjSQsD0OdGMr3X9kaGLEvNibwpUUvXcQd2WCerW3OOIWbs+ruW8SyHzYQcWsat6N
mAmhVqp0q0HOVDJ0Tr4vXRISXbZ4KpDXbOaIAeHzTauR+RRJehz7ZoWQA+QcT062
r+goT7WI691VCYGlo3UdC8Hkroo4490Hp2RE/pMRbIv7v71lKd60/pQ9IhB1vuI5
16SNtFsFosx1pg88AGW/f6HtAwI2Iowrl2EQqX9Wa5+7R2uArIdT35P0jXFY3mAt
p1RStDtSbiKdyOQVwZZ710tEh6gp0C+psSpVtuoTHcnFJXjRDujMZAVxWddCrW8Y
J37f5H0FQrbQGJLJxptkJgrggVWVWUbe7DBRqqZLL2AD3q1OTyVD9eby2m0mOAu5
/HVcBBtT85iuSF1Ie+FVcimK4WWODnKuA/yTRsEnw7sjNx7U48EEEfunsmEtncxB
Rrqtp8537D/2lxbsri9Zb1p4kHHcQJ6Gs/f3v2sJ2lqdRUpW+39r4L0=
=w0bv
-----END PGP PUBLIC KEY BLOCK-----
RELEASE_KEY
}

log() {
  printf '%s\n' "$1"
}

fail() {
  printf 'install: error: %s\n' "$1" >&2
  exit 1
}

fail_with_output() {
  cat "$1" >&2
  fail "$2"
}

has_command() {
  command -v "$1" >/dev/null 2>&1
}

remove_work_files() {
  [ -z "$staged_program" ] || rm -f "$staged_program"
  [ -z "$work_dir" ] || rm -rf "$work_dir"
}

select_release_file() {
  os_name=$(uname -s) || fail 'The uname command failed.'
  cpu_name=$(uname -m) || fail 'The uname command failed.'
  case "$os_name $cpu_name" in
    'Darwin '*) release_file="${PROGRAM_NAME}_darwin_universal" ;;
    'Linux x86_64' | 'Linux amd64') release_file="${PROGRAM_NAME}_linux_amd64" ;;
    'Linux aarch64' | 'Linux arm64') release_file="${PROGRAM_NAME}_linux_arm64" ;;
    *) fail "No release file exists for $os_name on $cpu_name. See $RELEASES_URL for the release files." ;;
  esac
}

# $1: exit status.
# After an exit in a signal trap, mksh ends with the status of the EXIT trap. So this clears that
# trap after a good removal. After a bad one, the EXIT trap tries again.
stop_on_signal() {
  remove_work_files && trap - EXIT
  exit "$1"
}

make_work_dir() {
  trap remove_work_files EXIT
  # dash and busybox ash run no EXIT trap on a signal. An older dash ends the
  # EXIT trap early when a signal trap exits. So each signal trap removes the files.
  trap 'stop_on_signal 129' HUP
  trap 'stop_on_signal 130' INT
  trap 'stop_on_signal 141' PIPE
  trap 'stop_on_signal 143' TERM
  work_dir=$(mktemp -d) || fail 'The installer cannot make a temporary directory.'
}

# --proto also limits redirects to HTTPS.
# No stdin for the tools: nothing waits for input.
download() {
  curl --proto '=https' --tlsv1.2 --connect-timeout 30 --retry 3 -fsSL \
    -o "$work_dir/$1" "$base_url/$VERSION/$1" </dev/null ||
    fail "The download of $base_url/$VERSION/$1 failed."
}

check_signature() {
  print_release_key >"$work_dir/release-key.asc" || fail 'The installer cannot write to its temporary directory.'
  if [ ! -s "$work_dir/release-key.asc" ]; then
    log "The signature check of $CHECKSUM_LIST was skipped. This installer has no release key."
    return 0
  fi
  for signature_tool in gpg gpgv; do
    if ! has_command "$signature_tool"; then
      log "The signature check of $CHECKSUM_LIST was skipped. $signature_tool was not found."
      return 0
    fi
  done
  download "$CHECKSUM_LIST.sig"
  # Own GnuPG home: the keys of the user stay unread and unchanged.
  # gpgv needs no key database and starts no daemon.
  mkdir -m 700 "$work_dir/gnupg" || fail 'The installer cannot write to its temporary directory.'
  gpg --homedir "$work_dir/gnupg" --batch --no-tty --no-options --dearmor \
    <"$work_dir/release-key.asc" >"$work_dir/release-key.gpg" 2>"$work_dir/gpg.log" ||
    fail_with_output "$work_dir/gpg.log" 'The release key in this installer is not valid.'
  gpgv --homedir "$work_dir/gnupg" --keyring "$work_dir/release-key.gpg" \
    "$work_dir/$CHECKSUM_LIST.sig" "$work_dir/$CHECKSUM_LIST" </dev/null >"$work_dir/gpgv.log" 2>&1 ||
    fail_with_output "$work_dir/gpgv.log" "The signature of $CHECKSUM_LIST is not good."
  log "The signature of $CHECKSUM_LIST is good."
}

check_checksum() {
  # Exact name in field 2. A name that only contains the file name is no match.
  expected_hash=$(awk -v name="$release_file" '$2 == name { print $1 }' "$work_dir/$CHECKSUM_LIST") ||
    fail "The installer cannot read $CHECKSUM_LIST."
  [ -n "$expected_hash" ] || fail "$CHECKSUM_LIST has no line for $release_file."
  if has_command sha256sum; then
    hash_output=$(sha256sum <"$work_dir/$release_file") || hash_output=''
  elif has_command shasum; then
    hash_output=$(shasum -a 256 <"$work_dir/$release_file") || hash_output=''
  else
    fail 'sha256sum and shasum were not found. The installer cannot check the download.'
  fi
  # Two lines for the name, or a value that is not a hash, never equal the hash of the file.
  [ "${hash_output%% *}" = "$expected_hash" ] ||
    fail "The SHA-256 checksum of $release_file is not the checksum in $CHECKSUM_LIST."
  log "The SHA-256 checksum of $release_file is correct."
}

# Without GnuPG, as on a standard macOS, only this check ties the program to the release team.
check_apple_signature() {
  [ "$os_name" = 'Darwin' ] || return 0
  has_command codesign || fail 'codesign was not found. The installer cannot check the Apple signature.'
  # -R takes a requirement text. The text starts with "=".
  # Check 1: an Apple certificate of the release team signed the file.
  # Without the team, a file from any Apple developer passes.
  codesign --verify -R"=anchor apple generic and certificate leaf[subject.OU] = \"$APPLE_TEAM_ID\"" \
    "$work_dir/$release_file" </dev/null >"$work_dir/codesign.log" 2>&1 ||
    fail_with_output "$work_dir/codesign.log" \
      "The Apple signature check of $release_file failed. The signer must be Apple Developer team $APPLE_TEAM_ID."
  # Check 2: Apple notarized the file.
  codesign --verify -R=notarized --check-notarization "$work_dir/$release_file" \
    </dev/null >"$work_dir/codesign.log" 2>&1 ||
    fail_with_output "$work_dir/codesign.log" "The Apple notarization check of $release_file failed."
  log "The Apple signature (team $APPLE_TEAM_ID) and notarization of $release_file are good."
}

install_program() {
  # ./ keeps cd away from CDPATH and from a name that starts with a dash.
  case "$install_dir" in
    /*) ;;
    *) install_dir="./$install_dir" ;;
  esac
  mkdir -p "$install_dir" || fail "The installer cannot make the directory $install_dir."
  install_dir=$(cd "$install_dir" && pwd) || fail "The installer cannot open the directory $install_dir."
  install_path="$install_dir/$PROGRAM_NAME"
  # mv would put the file into the directory.
  [ ! -d "$install_path" ] || fail "$install_path is a directory. Remove it, then run the installer again."
  replaced_file='no'
  if [ -e "$install_path" ]; then
    replaced_file='yes'
  fi
  # New file, then rename in the same directory. A bridge that runs keeps the old file.
  # A write into the old file can fail on Linux while a bridge runs (ETXTBSY), and macOS can
  # kill a program whose signed file changed in place.
  staged_program=$(mktemp "$install_dir/.$PROGRAM_NAME.XXXXXX") ||
    fail "The installer cannot write to $install_dir."
  cp "$work_dir/$release_file" "$staged_program" || fail "The installer cannot write to $install_dir."
  chmod 0755 "$staged_program" || fail "The installer cannot write to $install_dir."
  mv -f "$staged_program" "$install_path" || fail "The installer cannot write $install_path."
  staged_program=''
}

report_install() {
  log "Installed $PROGRAM_NAME $VERSION at $install_path."
  if [ "$replaced_file" = 'yes' ]; then
    log "A bridge that runs now keeps the earlier version until you run: $PROGRAM_NAME bridge stop"
  fi
  found_path=$(command -v "$PROGRAM_NAME" 2>/dev/null) || found_path=''
  # -ef: same file. POSIX.1-2024 has it, and so do dash, bash and busybox ash.
  # shellcheck disable=SC3013
  if [ -z "$found_path" ]; then
    hint="$install_dir is not on your PATH. To add it,"
  elif ! [ "$found_path" -ef "$install_path" ] 2>/dev/null; then
    hint="Your shell runs $found_path, not the new file. To put $install_dir first on your PATH,"
  else
    return 0
  fi
  # The user copies the printed commands. These characters change what a command does.
  case "$install_dir" in
    *[\'\"\$\`\\!]*)
      log "$hint edit the PATH setting of your shell."
      return 0
      ;;
  esac
  # SHELL names the login shell of the user. The shell that runs a command expands the file name.
  login_shell=${SHELL:-}
  # shellcheck disable=SC2016,SC2088
  case "${login_shell##*/} $os_name" in
    'zsh '*) profile_file='"${ZDOTDIR:-$HOME}/.zshrc"' ;;
    'bash Darwin')
      # A macOS terminal starts bash as a login shell. That shell reads only the first of
      # .bash_profile, .bash_login and .profile that exists.
      profile_file='~/.bash_profile'
      for profile_name in .profile .bash_login .bash_profile; do
        [ ! -e "${HOME:-}/$profile_name" ] || profile_file="~/$profile_name"
      done
      ;;
    'bash '*) profile_file='~/.bashrc' ;;
    *) profile_file='' ;;
  esac
  path_line="export PATH=\"$install_dir:\$PATH\""
  if [ -n "$profile_file" ]; then
    # Command 1: new terminals. Its first line feed keeps a last profile line that has none.
    # Command 2: this terminal.
    log "$hint run these commands:"
    log "  printf '\\n%s\\n' '$path_line' >> $profile_file"
    log "  $path_line"
  else
    log "$hint put this line in your shell profile, and run it in this terminal:"
    log "  $path_line"
  fi
}

main() {
  [ "$#" -eq 0 ] || fail 'The installer takes no arguments.'
  select_release_file
  has_command curl || fail 'curl was not found. Install curl, then run the installer again.'
  base_url=${DEVICEFARM_CONNECT_INSTALL_BASE_URL:-$DEFAULT_BASE_URL}
  base_url=${base_url%/}
  install_dir=${DEVICEFARM_CONNECT_INSTALL_DIR:-}
  if [ -z "$install_dir" ]; then
    [ -n "${HOME:-}" ] || fail 'HOME is not set. Set DEVICEFARM_CONNECT_INSTALL_DIR to the install directory.'
    install_dir="$HOME/.local/bin"
  fi
  make_work_dir
  log "Downloading $PROGRAM_NAME $VERSION ($release_file)."
  download "$CHECKSUM_LIST"
  check_signature
  download "$release_file"
  check_checksum
  check_apple_signature
  install_program
  report_install
}

main "$@"
