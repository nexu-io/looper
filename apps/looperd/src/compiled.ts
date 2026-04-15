import { resolveLooperdCliArgv, runLooperdCli } from "./index";

process.exit(await runLooperdCli(resolveLooperdCliArgv(process.argv)));
