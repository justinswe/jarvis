"""Rules for packaging Go binaries as container images."""

load("@rules_img//img:image.bzl", "image_from_binary")
load("@rules_img//img:layer.bzl", "image_layer")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:push.bzl", "image_push")

def go_container_image(
        name,
        binary,
        base,
        platform,
        registry,
        repository,
        tag = "latest",
        additional_tags = {},
        files = {},
        visibility = None):
    """Creates an image plus runnable load and push targets for a Go binary.

    The generated targets are `<name>`, `<name>_load`, and `<name>_push`.

    Args:
        name: Name of the OCI image target.
        binary: Go binary target to package.
        base: Base image target.
        platform: Target platform for the image.
        registry: Destination registry for the push target.
        repository: Destination repository and local image name.
        tag: Image tag for load and push operations.
        additional_tags: Mapping of target suffixes to additional image tags.
        files: Mapping of absolute container paths to source labels.
        visibility: Visibility applied to the generated public targets.
    """
    layers = []
    if files:
        layer_name = name + "_files"
        image_layer(
            name = layer_name,
            srcs = files,
            visibility = ["//visibility:private"],
        )
        layers.append(":" + layer_name)

    image_from_binary(
        name = name,
        base = base,
        binary = binary,
        include_runfiles = False,
        layers = layers,
        path = "/app/" + binary.split(":")[-1],
        platforms = [platform],
        visibility = visibility,
        working_dir = "/app",
    )

    image_load(
        name = name + "_load",
        image = ":" + name,
        tag = repository + ":" + tag,
        tags = ["manual"],
        visibility = visibility,
    )

    image_push(
        name = name + "_push",
        image = ":" + name,
        registry = registry,
        repository = repository,
        tag = tag,
        tags = ["manual"],
        visibility = visibility,
    )

    for suffix, additional_tag in additional_tags.items():
        image_push(
            name = name + "_push_" + suffix,
            image = ":" + name,
            registry = registry,
            repository = repository,
            tag = additional_tag,
            tags = ["manual"],
            visibility = visibility,
        )
