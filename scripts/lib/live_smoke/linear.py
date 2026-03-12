"""Linear GraphQL 辅助。"""

from __future__ import annotations

from dataclasses import dataclass
import json
from typing import Any
from urllib import request


ACTIVE_STATES = ["Todo", "In Progress"]


@dataclass
class TeamContext:
    team_id: str
    project_id: str
    todo_state_id: str
    done_state_id: str
    canceled_state_id: str


class LinearClient:
    def __init__(self, api_key: str):
        self.api_key = api_key

    def execute(self, query: str, variables: dict[str, Any] | None = None) -> dict[str, Any]:
        body = json.dumps({"query": query, "variables": variables or {}}, separators=(",", ":")).encode("utf-8")
        req = request.Request(
            "https://api.linear.app/graphql",
            data=body,
            headers={
                "Authorization": self.api_key,
                "Content-Type": "application/json",
            },
            method="POST",
        )
        with request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
        errors = payload.get("errors") or []
        if errors:
            messages = "; ".join(str(item.get("message", "unknown graphql error")) for item in errors)
            raise RuntimeError(f"linear graphql error: {messages}")
        data = payload.get("data")
        if not isinstance(data, dict):
            raise RuntimeError("linear graphql response is missing data")
        return data

    def load_team_context(self, team_key: str, project_slug: str) -> TeamContext:
        data = self.execute(
            """
            query($teamKey: String!) {
              teams(filter: { key: { eq: $teamKey } }) {
                nodes {
                  id
                  key
                  states { nodes { id name } }
                  projects { nodes { id slugId name } }
                }
              }
            }
            """,
            {"teamKey": team_key},
        )
        teams = data["teams"]["nodes"]
        if not teams:
            raise RuntimeError(f"linear team {team_key!r} not found")
        team = teams[0]
        project_id = ""
        for project in team["projects"]["nodes"]:
            if str(project.get("slugId", "")).strip() == project_slug:
                project_id = str(project["id"])
                break
        if not project_id:
            raise RuntimeError(f"linear project slug {project_slug!r} not found in team {team_key!r}")

        states = {str(node["name"]).strip(): str(node["id"]).strip() for node in team["states"]["nodes"]}
        todo_state_id = states.get("Todo", "")
        done_state_id = states.get("Done", "")
        canceled_state_id = states.get("Canceled", "") or states.get("Cancelled", "")
        if not todo_state_id or not done_state_id or not canceled_state_id:
            raise RuntimeError("linear workflow states Todo/Done/Canceled are required for live smoke")
        return TeamContext(
            team_id=str(team["id"]),
            project_id=project_id,
            todo_state_id=todo_state_id,
            done_state_id=done_state_id,
            canceled_state_id=canceled_state_id,
        )

    def create_issue(self, title: str, context: TeamContext) -> dict[str, Any]:
        data = self.execute(
            """
            mutation($title: String!, $teamId: String!, $projectId: String!, $stateId: String!) {
              issueCreate(input: { title: $title, teamId: $teamId, projectId: $projectId, stateId: $stateId }) {
                success
                issue { id identifier title state { name } }
              }
            }
            """,
            {
                "title": title,
                "teamId": context.team_id,
                "projectId": context.project_id,
                "stateId": context.todo_state_id,
            },
        )
        result = data["issueCreate"]
        if not result.get("success"):
            raise RuntimeError("linear issueCreate returned success=false")
        return result["issue"]

    def update_issue_state(self, issue_id: str, state_id: str) -> dict[str, Any]:
        data = self.execute(
            """
            mutation($id: String!, $stateId: String!) {
              issueUpdate(id: $id, input: { stateId: $stateId }) {
                success
                issue { id identifier title state { name } }
              }
            }
            """,
            {"id": issue_id, "stateId": state_id},
        )
        result = data["issueUpdate"]
        if not result.get("success"):
            raise RuntimeError("linear issueUpdate returned success=false")
        return result["issue"]

    def fetch_issue(self, issue_id: str) -> dict[str, Any]:
        data = self.execute(
            """
            query($id: String!) {
              issue(id: $id) {
                id
                identifier
                title
                state { name }
              }
            }
            """,
            {"id": issue_id},
        )
        issue = data.get("issue")
        if not isinstance(issue, dict):
            raise RuntimeError(f"linear issue {issue_id!r} not found")
        return issue

    def fetch_active_issues(self, project_slug: str) -> list[dict[str, Any]]:
        data = self.execute(
            """
            query($projectSlug: String!, $states: [String!]) {
              issues(
                first: 50,
                filter: {
                  project: { slugId: { eq: $projectSlug } }
                  state: { name: { in: $states } }
                }
              ) {
                nodes {
                  id
                  identifier
                  title
                  state { name }
                }
              }
            }
            """,
            {"projectSlug": project_slug, "states": ACTIVE_STATES},
        )
        return list(data["issues"]["nodes"])

    def fetch_smoke_issues(self, project_slug: str, title_prefix: str) -> list[dict[str, Any]]:
        data = self.execute(
            """
            query($projectSlug: String!, $titlePrefix: String!) {
              issues(
                first: 100,
                filter: {
                  project: { slugId: { eq: $projectSlug } }
                  title: { startsWith: $titlePrefix }
                }
              ) {
                nodes {
                  id
                  identifier
                  title
                  state { name }
                }
              }
            }
            """,
            {"projectSlug": project_slug, "titlePrefix": title_prefix},
        )
        return list(data["issues"]["nodes"])

    def archive_issue(self, issue_id: str, *, trash: bool = False) -> None:
        data = self.execute(
            """
            mutation($id: String!, $trash: Boolean) {
              issueArchive(id: $id, trash: $trash) {
                success
              }
            }
            """,
            {"id": issue_id, "trash": trash},
        )
        result = data["issueArchive"]
        if not result.get("success"):
            raise RuntimeError("linear issueArchive returned success=false")
