# ADR 0008: Zap e OpenTelemetry

- Status: aceito
- Data: 2026-09-05

## Contexto

Falhas em pagamentos atravessam HTTP, banco, broker e worker. Correlacionar
essas etapas desde a fundacao reduz o custo de diagnostico e impede que a
observabilidade seja adicionada tardiamente de forma inconsistente.

## Decisao

Logs estruturados usarao Zap. Desenvolvimento usa formato legivel e producao
usa JSON. Logs incluem `correlation_id`, `trace_id` e `span_id` quando
disponiveis, sem secrets ou dados completos de instrumentos de pagamento.

Traces e metricas usarao OpenTelemetry e serao exportados por OTLP. O ambiente
local podera habilitar o `grafana/otel-lgtm` por um profile do Docker Compose.
Esse container e exclusivo para desenvolvimento e testes; o backend OTLP de um
deploy e configurado externamente.

## Consequencias

- API e worker nascem com correlacao e shutdown dos exporters.
- Instrumentacao adiciona dependencias e algum custo de execucao.
- O destino da telemetria nao fica acoplado a um fornecedor.
- Exportacao OTLP pode ser desabilitada sem alterar os casos de uso.

