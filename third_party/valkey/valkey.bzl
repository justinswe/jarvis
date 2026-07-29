"""Pinned upstream valkey-server release binaries."""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

VERSION = "8.1.9"

# Valkey attaches no assets to its GitHub releases and publishes no macOS build at
# all, so these Linux tarballs from download.valkey.io are the only prebuilt server
# there is. They are keyed by the distribution they were built on rather than by
# platform; jammy links the oldest glibc of the choices, so it runs on the widest
# range of hosts. macOS falls back to a valkey-server on PATH, see jarvis/locate.go.
_PLATFORMS = {
    "linux_amd64": ("jammy-x86_64", "2d9282868c39e98ef636abf68b46a2c7741c4fbeeb4a0a1327070dbc4dce194f"),
    "linux_arm64": ("jammy-arm64", "d1b1e99f9e737ab42a720cca25f795c5c62648a1f79bd06f46381deca522f541"),
}

# The archive also carries valkey-cli and the sentinel and benchmark tools. Only the
# server is exposed, because only the server is what the supervisor starts.
_BUILD_FILE = """\
filegroup(
    name = "valkey-server",
    srcs = ["{prefix}/bin/valkey-server"],
    visibility = ["//visibility:public"],
)
"""

def _valkey_impl(_module_ctx):
    for name, (suffix, sha256) in _PLATFORMS.items():
        prefix = "valkey-{version}-{suffix}".format(version = VERSION, suffix = suffix)
        http_archive(
            name = "valkey_server_" + name,
            build_file_content = _BUILD_FILE.format(prefix = prefix),
            sha256 = sha256,
            urls = ["https://download.valkey.io/releases/{prefix}.tar.gz".format(prefix = prefix)],
        )

valkey = module_extension(implementation = _valkey_impl)
