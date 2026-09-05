# Estrutura do projeto

Este documento e a referencia para a responsabilidade de cada diretorio. Os
pacotes devem depender do dominio e dos casos de uso, nunca no sentido inverso.

```text
api/           contrato OpenAPI e configuracao do gerador
cmd/           pontos de entrada de api, worker e migrations
db/            migrations, queries SQL e configuracao do sqlc
deployments/   arquivos especificos de ambientes de implantacao
docs/          arquitetura, decisoes e guias do projeto
internal/      toda implementacao que nao constitui API publica Go
tests/         testes que atravessam mais de um pacote
.github/       automacoes de integracao, seguranca e releases
```

## Pacotes internos

```text
internal/domain/        entidades, valores e invariantes puras
internal/application/   casos de uso e portas exigidas por eles
internal/adapters/      PostgreSQL, RabbitMQ e provedores externos
internal/transport/     entradas HTTP e traducao de protocolos
internal/platform/      configuracao, banco, logs, lifecycle e telemetria
```

`internal/domain` nao importa HTTP, GORM, PostgreSQL, RabbitMQ, Stripe ou
OpenTelemetry. `internal/application` define as interfaces que os adapters
implementam. `cmd` somente carrega configuracao e compoe dependencias.

Codigo gerado fica proximo do adapter que o consome e nunca e editado
manualmente:

```text
internal/transport/http/openapi/    tipos e strict server do oapi-codegen
internal/adapters/postgres/queries/ queries geradas pelo sqlc
```

Diretorios `pkg/` e `utils/` nao devem ser criados sem um consumidor externo ou
uma responsabilidade claramente definida.

