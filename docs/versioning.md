# Versionamento

O Payments Boilerplate segue [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)
com tags no formato `vMAJOR.MINOR.PATCH`.

## API pública

O significado das versões depende de uma fronteira pública explícita.

Fazem parte da API pública:

- Endpoints e schemas descritos no OpenAPI.
- Códigos e formatos de erro documentados.
- Variáveis de ambiente e arquivos de configuração documentados.
- Comandos, argumentos e códigos de saída dos binários.
- Contratos RabbitMQ documentados para consumo externo.
- Pacotes Go eventualmente publicados fora de `internal/`.

Não fazem parte da API pública:

- Estrutura de pacotes em `internal/`.
- Schema e queries do PostgreSQL.
- Código gerado pelo OpenAPI ou `sqlc`.
- Implementação dos adapters.
- Topologia e mensagens marcadas explicitamente como internas.

## Série `v0.x`

Enquanto o projeto estiver abaixo de `v1.0.0`, seu status é experimental.

- `PATCH` corrige comportamento preservando compatibilidade dentro da minor.
- `MINOR` adiciona funcionalidade e pode conter breaking changes.
- Breaking changes devem aparecer claramente no changelog e nas notas da
  release.
- Não há garantia de compatibilidade entre duas versões minor da série `v0.x`.

Exemplos:

```text
v0.1.0 -> v0.1.1  correção compatível
v0.1.1 -> v0.2.0  funcionalidade ou mudança incompatível
```

A primeira fatia vertical completa será `v0.1.0`. Antes dela, podem ser
publicadas pré-releases:

```text
v0.1.0-alpha.1
v0.1.0-beta.1
v0.1.0-rc.1
v0.1.0
```

## A partir de `v1.0.0`

- `PATCH`: correção retrocompatível.
- `MINOR`: funcionalidade nova e retrocompatível.
- `MAJOR`: mudança incompatível na API pública.

Breaking changes devem oferecer um caminho de migração. Quando possível, uma
funcionalidade será marcada como deprecated em uma minor antes de ser removida
na major seguinte.

## Versões HTTP, OpenAPI e release

São versões relacionadas, mas com funções diferentes:

```text
Release do projeto: v0.3.0
Versão no OpenAPI:  0.3.0
Prefixo HTTP:       /v1
```

O prefixo muda para `/v2` apenas quando for necessário manter uma geração
incompatível da API HTTP. Durante `v0.x`, o prefixo `/v1` não representa uma
promessa de estabilidade; o status experimental continua prevalecendo.

## Mensagens

Cada envelope possui `schemaVersion`. Mudanças aditivas e opcionais preservam a
versão. Remoção, renomeação ou mudança semântica de campos cria uma nova versão
do schema e, quando necessário, uma nova routing key.

## Módulos Go

Enquanto não houver pacotes públicos, o repositório é versionado como uma única
unidade. Se um módulo público chegar à major `v2`, seu module path também deverá
conter `/v2`, conforme o semantic import versioning do Go.

## Releases

Cada release deve:

1. Ter CI completo aprovado.
2. Atualizar o `CHANGELOG.md`.
3. Declarar breaking changes e passos de migração.
4. Informar versões testadas de Go, PostgreSQL e RabbitMQ.
5. Gerar artefatos a partir do commit marcado pela tag.
6. Manter tags imutáveis; uma release incorreta recebe uma nova versão.

