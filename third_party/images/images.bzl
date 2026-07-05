"""Pinned third-party container images."""

load("@rules_img//img:pull.bzl", "pull")

def _images_impl(_module_ctx):
    pull(
        name = "distroless_base_debian13_nonroot",
        digest = "sha256:ced0a2b1936b14d5bddc2ee02a807b1586ca6576a967f5b043f4a3301c8a8f6b",
        registry = "gcr.io",
        repository = "distroless/base-debian13",
        tag = "nonroot",
    )

images = module_extension(implementation = _images_impl)
