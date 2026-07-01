# Ray cluster observability: dashboard + Prometheus + Grafana

This follows the **official KubeRay monitoring guide**
(https://docs.ray.io/en/latest/cluster/kubernetes/k8s-ecosystem/prometheus-grafana.html),
not a hand-rolled Prometheus. It installs the `kube-prometheus-stack` (Prometheus
Operator + Grafana + Alertmanager + kube-state-metrics + node-exporter), applies
KubeRay's `PodMonitor`s and `PrometheusRule`, and auto-loads Ray's Grafana
dashboards.

## Ray dashboard (built-in, no install)

The RayCluster head already serves the Ray dashboard and a Prometheus metrics
endpoint (see `deploy/ray/raycluster.yaml`, service `zz-ray-head-svc`):

```sh
kubectl -n zumble-zay port-forward svc/zz-ray-head-svc 8265:8265   # dashboard
# open http://localhost:8265  (jobs, actors — incl. the llm-rank Scorer actors)

kubectl -n zumble-zay port-forward svc/zz-ray-head-svc 8080:8080   # raw metrics
curl localhost:8080/metrics                                        # ray_* series
```

## Prometheus + Grafana (official KubeRay install)

The KubeRay repo ships the install assets. Clone it and run the installer, then
point the PodMonitors at this project's namespace (`zumble-zay`, not the guide's
`default`):

```sh
git clone --depth 1 https://github.com/ray-project/kuberay.git
cd kuberay

# PodMonitors default to namespace "default"; this project's RayCluster is in
# "zumble-zay", so retarget them before the installer applies them.
sed -i '' 's/      - default/      - zumble-zay/' config/prometheus/podMonitor.yaml

# Installs kube-prometheus-stack v48.2.1 into namespace prometheus-system,
# applies config/prometheus/podMonitor.yaml + rules, and auto-loads Ray's
# Grafana dashboards.
./install/prometheus/install.sh --auto-load-dashboard true
```

The installer creates two `PodMonitor`s — `ray-head-monitor` and
`ray-workers-monitor` — which scrape the Ray pods' `:8080/metrics`, and a
`PrometheusRule` (`ray-cluster-gcs-rules`).

### Access

```sh
# Prometheus
kubectl -n prometheus-system port-forward svc/prometheus-kube-prometheus-prometheus 9090:9090
# http://localhost:9090 — try:  ray_component_cpu_percentage  or  ray_tasks

# Grafana (user: admin; password below)
kubectl -n prometheus-system get secret prometheus-grafana -o jsonpath='{.data.admin-password}' | base64 -d; echo
kubectl -n prometheus-system port-forward svc/prometheus-grafana 3000:80
# http://localhost:3000 — dashboards: "Ray", "Serve", "Serve LLM", "Train",
# "Data", "KubeRay Operator", ...
```

### Verify the Ray targets are up

```sh
# Both should show health "up":
curl -s localhost:9090/api/v1/targets \
  | python3 -c 'import sys,json;[print(t["scrapePool"],t["health"]) for t in json.load(sys.stdin)["data"]["activeTargets"] if "ray" in t["scrapePool"]]'
```

## Notes

- This is a **heavy** footprint (Grafana, operator, alertmanager, node-exporter,
  kube-state-metrics). On the local podman VM, ensure it has enough memory
  (`podman machine set --memory 12288`).
- Per-actor series are visible: the llm-rank actors path (docs/adr/0029) shows up
  as `ray::Scorer.score` components in `ray_component_*` metrics.
- Teardown: `helm -n prometheus-system uninstall prometheus` and
  `kubectl delete ns prometheus-system`.
