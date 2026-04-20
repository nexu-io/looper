import { describe, expect, test } from "bun:test";
import {
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { dirname, extname, join, resolve } from "node:path";

import { loadLooperConfig } from "./index";

interface ConfigParityFixture {
  description: string;
  input: {
    optionConfigPath?: string;
    defaultConfigPath?: string;
    argv?: string[];
    env?: Record<string, string>;
    files?: Record<string, unknown>;
  };
  expected: {
    config: Record<string, unknown>;
    metadata: Record<string, unknown>;
  };
}

const fixtureDir = resolve(
  import.meta.dir,
  "../../../../internal/config/testdata/parity",
);
const entries = (await readdir(fixtureDir)).filter(
  (entry) => extname(entry) === ".json",
);

describe("loadLooperConfig parity fixtures", () => {
  for (const entry of entries) {
    test(entry.replace(/\.json$/, ""), async () => {
      const rootDir = await mkdtemp(join(tmpdir(), "looper-config-parity-"));
      try {
        const fixture = await readParityFixture(join(fixtureDir, entry));

        await writeParityFiles(rootDir, fixture.input.files ?? {});

        const loaded = await loadLooperConfig({
          cwd: rootDir,
          argv: resolveParityStrings(fixture.input.argv ?? [], rootDir),
          env: resolveParityStringRecord(fixture.input.env ?? {}, rootDir),
          defaultConfigPath: resolveOptionalParityString(
            fixture.input.defaultConfigPath,
            rootDir,
          ),
        });

        const actual = {
          config: loaded.config,
          metadata: {
            configPath: loaded.metadata.configPath,
            configFilePresent: loaded.metadata.configFilePresent,
            toolDetection: loaded.metadata.toolDetection,
          },
        };

        const expected = resolveParityValue(
          {
            config: fixture.expected.config,
            metadata: fixture.expected.metadata,
          },
          rootDir,
        ) as typeof actual;

        expect(actual).toStrictEqual(expected);
      } finally {
        await rm(rootDir, { recursive: true, force: true });
      }
    });
  }
});

async function readParityFixture(path: string): Promise<ConfigParityFixture> {
  return JSON.parse(await readFile(path, "utf8")) as ConfigParityFixture;
}

async function writeParityFiles(
  rootDir: string,
  files: Record<string, unknown>,
): Promise<void> {
  for (const [relativePath, contents] of Object.entries(files)) {
    const targetPath = resolveFilePath(relativePath, rootDir);
    await mkdir(dirname(targetPath), { recursive: true });
    await writeFile(
      targetPath,
      JSON.stringify(resolveParityValue(contents, rootDir), null, 2),
    );
  }
}

function resolveParityValue(value: unknown, rootDir: string): unknown {
  if (typeof value === "string") {
    return resolveRequiredParityString(value, rootDir);
  }

  if (Array.isArray(value)) {
    return value.map((item) => resolveParityValue(item, rootDir));
  }

  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.entries(value).map(([key, entryValue]) => [
        key,
        resolveParityValue(entryValue, rootDir),
      ]),
    );
  }

  return value;
}

function resolveParityStrings(values: string[], rootDir: string): string[] {
  return values.map((value) => resolveRequiredParityString(value, rootDir));
}

function resolveParityStringRecord(
  values: Record<string, string>,
  rootDir: string,
): Record<string, string> {
  return Object.fromEntries(
    Object.entries(values).map(([key, value]) => [
      key,
      resolveRequiredParityString(value, rootDir),
    ]),
  );
}

function resolveRequiredParityString(value: string, rootDir: string): string {
  return resolveOptionalParityString(value, rootDir) ?? value;
}

function resolveOptionalParityString(
  value: string | undefined,
  rootDir: string,
): string | undefined {
  return value
    ?.replaceAll("__TMP__", rootDir)
    .replaceAll("__HOME__", homedir());
}

function resolveFilePath(relativePath: string, rootDir: string): string {
  const resolved = resolveRequiredParityString(relativePath, rootDir);
  return resolve(rootDir, resolved);
}
