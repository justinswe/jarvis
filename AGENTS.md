# Jarvis

This is a Bazel monorepo.
Jarvis is a fast, open-source AI chatbot for Discord.
It provides built-in search and uses Google Vertex AI to generate responses.

# Bazel

bazel build //package:target
bazel build //package/...
bazel test //package/...

<!-- Add Missing Dependencies -->

bazel run //:gazelle -- update PATH
bazel run //:gazelle

# Configuration

- Declare operator configuration as Cobra flags and execute commands through `app.RunCobraCommand`.
- The `app` package automatically maps flags to uppercase environment variables with hyphens replaced by underscores and loads supported `.env` files. For example, `--openrouter-api-key` maps to `OPENROUTER_API_KEY`.
- Application and manual-harness code must use the bound flag value. Do not read configuration with `os.Getenv`, `os.LookupEnv`, or equivalent APIs.
- Use purpose-built APIs for runtime infrastructure, such as rules_go's runfiles package for Bazel test data, instead of reading Bazel environment variables directly.

# Go Style Guide

https://google.github.io/styleguide/go/guide

[Overview](https://google.github.io/styleguide/go/index) \| [Guide](https://google.github.io/styleguide/go/guide) \| [Decisions](https://google.github.io/styleguide/go/decisions) \|
[Best practices](https://google.github.io/styleguide/go/best-practices)

## Important

1. Code Should always prioritize, readability, and maintainability.
2. Utilize small purposeful functions, with single-line doc strings.
3. Utilize guard clauses, and avoid nested conditionals.

**Note:** This is part of a series of documents that outline [Go Style](https://google.github.io/styleguide/go/index)
at Google. This document is **[normative](https://google.github.io/styleguide/go/index#normative) and**
**[canonical](https://google.github.io/styleguide/go/index#canonical)**. See [the overview](https://google.github.io/styleguide/go/index#about) for more
information.

## Style principles [Anchor](https://google.github.io/styleguide/go/guide#style-principles)

There are a few overarching principles that summarize how to think about writing
readable Go code. The following are attributes of readable code, in order of
importance:

1. **[Clarity](https://google.github.io/styleguide/go/guide#clarity)**: The code’s purpose and rationale is clear to the reader.
2. **[Simplicity](https://google.github.io/styleguide/go/guide#simplicity)**: The code accomplishes its goal in the simplest way
   possible.
3. **[Concision](https://google.github.io/styleguide/go/guide#concision)**: The code has a high signal-to-noise ratio.
4. **[Maintainability](https://google.github.io/styleguide/go/guide#maintainability)**: The code is written such that it can be easily
   maintained.
5. **[Consistency](https://google.github.io/styleguide/go/guide#consistency)**: The code is consistent with the broader Google codebase.
