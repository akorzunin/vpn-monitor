import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Activity, Play, RefreshCw, LogOut, ShieldCheck } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

type Sample = {
  target: string;
  ok: boolean;
  http_status: number;
  latency_ms: number;
  error?: string;
};
type Result = {
  route_id: string;
  started: string;
  finished: string;
  state: string;
  error?: string;
  controls: Sample[];
  samples: Sample[];
  failure_percent: number;
  latency_ms: number;
  p95_ms: number;
  udp: {
    state: string;
    sent: number;
    received: number;
    loss_percent: number;
    rtt_ms: number;
    p95_ms: number;
    error?: string;
  };
  speed: {
    state: string;
    bytes: number;
    seconds: number;
    mbps: number;
    error?: string;
  };
};
type Route = {
  id: string;
  server: string;
  protocol: string;
  udp: boolean;
  busy: boolean;
  stale: boolean;
  latest: Result | null;
};
function Status({ state }: { state: string }) {
  const color =
    state === "healthy" || state === "ok"
      ? "border-emerald-200 bg-emerald-50 text-emerald-800"
      : state === "down" || state === "error" || state === "failed"
        ? "border-red-200 bg-red-50 text-red-800"
        : "bg-muted text-muted-foreground";
  return <Badge className={color}>{state}</Badge>;
}
const number = (n: number, unit: string) => `${n.toFixed(1)} ${unit}`;

export default function App() {
  // Keep the bearer token in memory, never URLs, cookies or persistent browser storage.
  const [token, setToken] = useState("");
  const [input, setInput] = useState("");
  const [routes, setRoutes] = useState<Route[]>([]);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState(false);
  const [selected, setSelected] = useState("");
  const [history, setHistory] = useState<Result[]>([]);
  const [updated, setUpdated] = useState("");
  const api = useCallback(
    async (path: string, body?: unknown) => {
      const response = await fetch(path, {
        method: body ? "POST" : "GET",
        headers: {
          Authorization: `Bearer ${token}`,
          ...(body ? { "Content-Type": "application/json" } : {}),
        },
        body: body ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(10000),
      });
      if (!response.ok)
        throw new Error(
          response.status === 401
            ? "Invalid API token."
            : response.status === 409
              ? "This route is already queued or testing."
              : `Request failed (${response.status}).`,
        );
      return response.status === 202 ? null : response.json();
    },
    [token],
  );
  const refresh = useCallback(async () => {
    try {
      const data: Route[] = await api("/api/routes");
      setRoutes(data);
      setConnected(true);
      setUpdated(new Date().toLocaleTimeString());
      setError("");
      if (selected)
        setHistory(
          await api(`/api/history?route=${encodeURIComponent(selected)}`),
        );
    } catch (e) {
      setError(
        e instanceof Error
          ? e.message
          : "Cannot reach monitor. Displayed results may be outdated.",
      );
      setConnected(false);
    }
  }, [api, selected]);
  useEffect(() => {
    if (!token) return;
    void refresh();
    const timer = setInterval(() => void refresh(), 5000);
    return () => clearInterval(timer);
  }, [token, refresh]);
  async function test(id: string) {
    try {
      await api("/api/test", { route: id });
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Test failed");
    }
  }
  function login(e: FormEvent) {
    e.preventDefault();
    setToken(input);
    setInput("");
  }
  if (!token)
    return (
      <main className="mx-auto mt-24 max-w-md px-4">
        <Card>
          <CardHeader>
            <ShieldCheck className="mb-3 size-8 text-primary" />
            <CardTitle>VPN monitor</CardTitle>
            <p className="text-sm text-muted-foreground">
              Connect to your local checker. Telegram is not required.
            </p>
          </CardHeader>
          <CardContent>
            <form onSubmit={login} className="space-y-4">
              <label className="text-sm" htmlFor="token">
                API token
              </label>
              <Input
                id="token"
                type="password"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                required
                autoComplete="off"
              />
              <Button className="w-full" type="submit">
                Connect
              </Button>
              <p className="text-xs text-muted-foreground">
                Token stays in this tab's memory. Use HTTPS for remote access.
              </p>
            </form>
          </CardContent>
        </Card>
      </main>
    );
  const current = routes.find((r) => r.id === selected);
  const latest = current?.latest;
  return (
    <main className="mx-auto max-w-6xl space-y-6 px-4 py-8 md:px-8">
      <header className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold">
            <Activity className="text-primary" />
            VPN monitor
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Workstation → VPN route → test destination
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => void refresh()}>
            <RefreshCw />
            Refresh
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              setToken("");
              setRoutes([]);
              setHistory([]);
              setSelected("");
              setError("");
              setConnected(false);
            }}
          >
            <LogOut />
            Disconnect
          </Button>
        </div>
      </header>
      {error && (
        <div
          role="alert"
          className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800"
        >
          {error}
        </div>
      )}
      <div className="flex flex-wrap gap-4 text-sm text-muted-foreground">
        <span>{routes.length} routes</span>
        <span>
          {
            routes.filter((r) => r.latest?.state === "healthy" && !r.stale)
              .length
          }{" "}
          healthy
        </span>
        <span>{routes.filter((r) => r.busy).length} queued/testing</span>
        <span>
          {connected
            ? `Updated ${updated}`
            : "Disconnected · results may be outdated"}
        </span>
      </div>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {routes.map((route) => (
          <Card
            key={route.id}
            className={selected === route.id ? "ring-2 ring-primary" : ""}
          >
            <CardHeader>
              <div className="flex items-start justify-between gap-2">
                <div>
                  <CardTitle>{route.server}</CardTitle>
                  <p className="mt-1 text-sm text-muted-foreground">
                    {route.protocol}
                  </p>
                </div>
                <Status
                  state={
                    route.stale ? "stale" : (route.latest?.state ?? "unknown")
                  }
                />
              </div>
            </CardHeader>
            <CardContent className="space-y-4">
              <dl className="grid grid-cols-2 gap-x-3 gap-y-2 text-sm">
                <dt className="text-muted-foreground">HTTP latency</dt>
                <dd>
                  {route.latest?.samples.some((s) => s.ok)
                    ? number(route.latest.latency_ms, "ms")
                    : "—"}
                </dd>
                <dt className="text-muted-foreground">HTTP failures</dt>
                <dd>
                  {route.latest?.samples.length
                    ? number(route.latest.failure_percent, "%")
                    : "—"}
                </dd>
                <dt className="text-muted-foreground">UDP RTT / loss</dt>
                <dd>
                  {route.latest?.udp.sent
                    ? `${route.latest.udp.received ? number(route.latest.udp.rtt_ms, "ms") : "—"} / ${number(route.latest.udp.loss_percent, "%")}`
                    : route.udp
                      ? (route.latest?.udp.state ?? "not tested")
                      : "unsupported"}
                </dd>
                <dt className="text-muted-foreground">Download</dt>
                <dd>
                  {route.latest?.speed.state === "ok"
                    ? number(route.latest.speed.mbps, "Mbps")
                    : (route.latest?.speed.state ?? "not tested")}
                </dd>
              </dl>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  disabled={route.busy || !connected}
                  onClick={() => void test(route.id)}
                >
                  <Play />
                  {route.busy ? "Queued / testing" : "Run test"}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setHistory([]);
                    setSelected(route.id);
                  }}
                >
                  Details
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                {route.latest
                  ? new Date(route.latest.finished).toLocaleString()
                  : "Waiting for first result"}
              </p>
            </CardContent>
          </Card>
        ))}
      </div>
      {current && (
        <Card>
          <CardHeader>
            <CardTitle>{current.id} · detailed results</CardTitle>
            <p className="text-sm text-muted-foreground">
              HTTP latency is time to response headers. UDP ping is
              authenticated echo RTT, not ICMP.
            </p>
          </CardHeader>
          <CardContent className="space-y-5">
            {latest ? (
              <>
                <div className="flex flex-wrap gap-5 text-sm">
                  <span>
                    HTTP p95:{" "}
                    {latest.samples.some((s) => s.ok)
                      ? number(latest.p95_ms, "ms")
                      : "—"}
                  </span>
                  <span>
                    UDP: {latest.udp.state}
                    {latest.udp.sent > 0 &&
                      ` · ${latest.udp.received}/${latest.udp.sent} replies · p95 ${latest.udp.received ? number(latest.udp.p95_ms, "ms") : "—"}`}
                  </span>
                  <span>
                    Transfer: {number(latest.speed.bytes / 1048576, "MiB")} in{" "}
                    {number(latest.speed.seconds, "s")}
                  </span>
                </div>
                {[latest.error, latest.udp.error, latest.speed.error]
                  .filter(Boolean)
                  .map((e, i) => (
                    <p key={i} className="text-sm text-red-700">
                      {e}
                    </p>
                  ))}
                <div className="overflow-x-auto">
                  <table className="w-full text-left text-sm">
                    <caption className="pb-3 text-left font-medium">
                      Latest HTTP samples and direct controls
                    </caption>
                    <thead>
                      <tr className="border-b text-muted-foreground">
                        <th className="p-2">Kind / target</th>
                        <th>Result</th>
                        <th>HTTP</th>
                        <th>Latency</th>
                      </tr>
                    </thead>
                    <tbody>
                      {[
                        ...latest.controls.map((s) => ({
                          ...s,
                          kind: "Direct",
                        })),
                        ...latest.samples.map((s) => ({
                          ...s,
                          kind: "Tunnel",
                        })),
                      ].map((sample, i) => (
                        <tr key={i} className="border-b">
                          <td className="p-2">
                            {sample.kind} / {sample.target}
                          </td>
                          <td>
                            {sample.ok ? "OK" : (sample.error ?? "Failed")}
                          </td>
                          <td>{sample.http_status || "—"}</td>
                          <td>{number(sample.latency_ms, "ms")}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </>
            ) : (
              <p>No completed test yet.</p>
            )}
            <details>
              <summary className="cursor-pointer text-sm font-medium">
                Recent history ({history.length} runs)
              </summary>
              <div className="mt-3 max-h-72 overflow-auto text-sm">
                {history.map((h) => (
                  <div
                    className="flex flex-wrap justify-between gap-2 border-b py-2"
                    key={h.started}
                  >
                    <span>{new Date(h.finished).toLocaleString()}</span>
                    <Status state={h.state} />
                    <span>
                      {number(h.latency_ms, "ms")} ·{" "}
                      {number(h.failure_percent, "% HTTP failures")} ·{" "}
                      {h.speed.state === "ok"
                        ? number(h.speed.mbps, "Mbps")
                        : h.speed.state}
                    </span>
                  </div>
                ))}
              </div>
            </details>
          </CardContent>
        </Card>
      )}
      <footer className="text-xs leading-relaxed text-muted-foreground">
        Failed direct controls make VPN results inconclusive. Host TUN/routing
        can affect all paths. Speed is a bounded sample, not sustained capacity.
        No automatic switching or server replacement.
      </footer>
    </main>
  );
}
