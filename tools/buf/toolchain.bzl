"""Buf toolchains for remote (linux) execution platforms.

rules_buf's own extension downloads buf only for the host that fetched it and
registers toolchains exec-compatible with that host alone, so any build whose
only execution platform is linux (--config=bb / --config=rbe) cannot resolve
the buf toolchain types. These wrap the official linux release binaries with
the same ToolchainInfo shape rules_buf produces.
"""

def _buf_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(cli = ctx.executable.cli)]

buf_toolchain = rule(
    implementation = _buf_toolchain_impl,
    attrs = {
        "cli": attr.label(
            doc = "The buf cli",
            executable = True,
            allow_single_file = True,
            mandatory = True,
            cfg = "exec",
        ),
    },
)
