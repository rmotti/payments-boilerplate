# Observabilidade local

O profile `observability` do `compose.yaml` inicia o Grafana OpenTelemetry LGTM
para desenvolvimento e testes. Ele nao e uma configuracao de producao.

```bash
docker compose --profile observability up -d
```

Grafana fica disponivel em `http://localhost:3000` e o endpoint OTLP HTTP em
`http://localhost:4318`.

