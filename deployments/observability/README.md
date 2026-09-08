# Observabilidade local

O profile `observability` do `compose.yaml` inicia o Grafana OpenTelemetry LGTM
para desenvolvimento e testes. Ele nao e uma configuracao de producao.

```bash
make observability-up
```

Grafana fica disponivel em `http://localhost:3000` e o endpoint OTLP HTTP em
`http://localhost:4318`. Para a aplicacao exportar, defina `OTEL_ENABLED=true`.

## Dashboard versionado

`payments-pipeline.json` cobre HTTP, provedor, inbox, outbox, retry, DLQ e pool
do PostgreSQL. Ele e provisionado automaticamente pelo profile acima, montado
somente leitura junto de `dashboards.yaml`: uma edicao feita na interface do
Grafana nao vira fonte de verdade do arquivo versionado.

Em um deploy real, o destino OTLP e configuracao externa. Importe este JSON pelo
mecanismo do backend usado; ele depende apenas de um datasource Prometheus, sem
nome fixo, escolhido pela variavel `datasource` do proprio dashboard.

O dashboard consulta apenas instrumentos que a aplicacao publica e apenas
labels da allowlist do [ADR 0015](../../docs/decisions/0015-application-metrics-and-cardinality.md).
Um teste em `internal/platform/metrics` verifica as duas coisas na CI, de modo
que um painel permanentemente vazio ou uma label proibida sao detectados antes
de um incidente.

A variavel `service` filtra por processo: `payments-api`, `payments-worker` ou
ambos. Com mais de uma instancia de worker, os paineis de backlog e de fila
agregam com `max`, porque todas as instancias amostram o mesmo estado.

Consulte [docs/metrics.md](../../docs/metrics.md) para o significado de cada
instrumento e as referencias operacionais iniciais.
