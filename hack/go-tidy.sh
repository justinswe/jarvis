bazel run @rules_go//go -- mod tidy
bazel mod deps --lockfile_mode=update
bazel mod tidy
