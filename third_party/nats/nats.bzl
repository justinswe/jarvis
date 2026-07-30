"""Pinned upstream nats-server release binaries."""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

VERSION = "2.14.3"

# Release archive checksums, keyed by the upstream platform suffix.
_SHA256 = {
    "darwin-amd64": "effa518fe623433d7c3321991db419bb1e00aa9b19ae761b559b580434afc3ab",
    "darwin-arm64": "e086395457a7a93a440433a446c9f161f917d35c0374640555123e8d8d4b7fe9",
    "linux-amd64": "f3d0c820c749f81d717310fb00d4903919e70e3e66b268bd352a088b9788eb93",
    "linux-arm64": "1759b6a0ddebade9471b7c02891dfaa8c73b526c6f3ce391d4e21ec3eceffab8",
}

_BUILD_FILE = """\
filegroup(
    name = "nats-server",
    srcs = ["{prefix}/nats-server"],
    visibility = ["//visibility:public"],
)
"""

def _nats_impl(_module_ctx):
    for platform, sha256 in _SHA256.items():
        prefix = "nats-server-v{version}-{platform}".format(version = VERSION, platform = platform)
        http_archive(
            name = "nats_server_" + platform.replace("-", "_"),
            build_file_content = _BUILD_FILE.format(prefix = prefix),
            sha256 = sha256,
            urls = ["https://github.com/nats-io/nats-server/releases/download/v{version}/{prefix}.tar.gz".format(
                version = VERSION,
                prefix = prefix,
            )],
        )

nats = module_extension(implementation = _nats_impl)
