"""Harbor adapter that executes the real Bolt headless agent in a task."""

import os
import shlex
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class Bolt(BaseInstalledAgent):
    """Run Bolt's normal CLI/tool/provider path inside Harbor's task image."""

    @staticmethod
    def name() -> str:
        return "bolt"

    def version(self) -> str | None:
        return os.environ.get("BOLT_VERSION", "source")

    async def install(self, environment: BaseEnvironment) -> None:
        source = os.environ.get("BOLT_BINARY")
        if not source:
            raise RuntimeError("BOLT_BINARY must point to the built Bolt binary")
        binary = Path(source).resolve()
        if not binary.is_file():
            raise FileNotFoundError(binary)
        await environment.upload_file(binary, "/usr/local/bin/bolt")
        await self.exec_as_root(environment, command="chmod 0755 /usr/local/bin/bolt")

    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        del context
        # Explicit flags isolate task runs from ~/.agenterm and use Bolt's
        # actual permissions, tools, agent loop, and provider path.
        flags = ["--no-resume", "--no-mcp", "--workspace", "."]
        for flag, env_name in (("--base-url", "AGENTERM_BASE_URL"),
                               ("--model", "AGENTERM_MODEL"),
                               ("--api-key", "AGENTERM_API_KEY")):
            if os.environ.get(env_name):
                flags += [flag, os.environ[env_name]]
        command = "bolt " + " ".join(shlex.quote(x) for x in flags)
        command += " exec " + shlex.quote(instruction)
        result = await self.exec_as_agent(environment, command=command)
        self.logger.info("bolt stdout:\n%s", result.stdout or "")
        if result.stderr:
            self.logger.info("bolt stderr:\n%s", result.stderr)
