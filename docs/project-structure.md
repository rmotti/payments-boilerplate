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
tests/         testes ponta a ponta e de falhas entre processos e adapters
.github/       automacoes de integracao, seguranca e releases
```

## Pacotes internos

```text
internal/domain/        entidades, valores e invariantes puras
internal/application/   casos de uso e portas exigidas por eles
internal/adapters/      PostgreSQL, RabbitMQ, catalogo e provedores externos
internal/transport/     entradas HTTP e traducao de protocolos
internal/platform/      configuracao, banco, logs, lifecycle e telemetria
internal/runtime/       composicao executavel de cada processo
```

`internal/domain` nao importa HTTP, GORM, PostgreSQL, RabbitMQ, Stripe ou
OpenTelemetry. `internal/application` define as interfaces que os adapters
implementam.

A composicao de cada processo vive em `internal/runtime/api` e
`internal/runtime/worker`, e nao no `cmd` correspondente. Um pacote `main` nao e
importavel, entao um teste de fluxo completo nao teria como subir a mesma
montagem que o binario sobe. Cada pacote expoe `Run` recebendo contexto,
configuracao e um `Options` de campos opcionais, tipados pelas portas que a
propria aplicacao ja define. `cmd` fica com o que e responsabilidade de
processo: carregar configuracao, transformar sinal em contexto cancelado e
mapear falha em codigo de saida. Ver
[ADR 0014](decisions/0014-testable-composition-and-e2e-boundaries.md).

Codigo gerado fica proximo do adapter que o consome e nunca e editado
manualmente:

```text
internal/transport/http/openapi/    tipos e strict server do oapi-codegen
internal/adapters/postgres/queries/ queries geradas pelo sqlc
```

O catalogo de produtos vive em `internal/adapters/catalog`. Na versao 0.1 ele e
fixo e em memoria, mas ja fica atras da porta `Catalog` do caso de uso, entao um
catalogo persistido ou remoto entra como outro adapter sem tocar o dominio.

Os testes de integração PostgreSQL ficam junto dos repositories que exercitam.
O diretório `tests/e2e` atravessa API, worker, PostgreSQL e RabbitMQ usando a
mesma composição dos binários; `tests/chaos` cobre interrupções determinísticas
e recuperação de dependências reais. Os comandos e requisitos estão em
[`tests/README.md`](../tests/README.md).

Diretorios `pkg/` e `utils/` nao devem ser criados sem um consumidor externo ou
uma responsabilidade claramente definida.
