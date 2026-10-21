# ext/skills conformance

Cases live in `conformance/loop/ext/skills/`. A case names the extension in `agent.extensions` as `"skills": options`, where `files` maps paths to file contents. The extension then has one source: a file-system source (`README.md`) over an in-memory file system holding exactly those files, its root the file system's root. Everything else takes the extension's defaults.
