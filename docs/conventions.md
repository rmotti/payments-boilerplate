# Convenções do projeto

Estas convenções existem para manter o Payments Boilerplate previsível para
quem usa e para quem contribui. Exceções devem ser justificadas no pull request
e, quando alterarem uma decisão estrutural, registradas em um ADR.

## Go

- Todo código deve passar por `gofmt`.
- `go vet`, testes e linters configurados pelo projeto devem passar no CI.
- Código que não constitui API pública deve permanecer em `internal/`.
- Pacotes em `pkg/` só serão criados quando houver um consumidor externo real.
- `context.Context` deve ser o primeiro argumento de operações que realizam I/O.
- Chamadas ao PostgreSQL, RabbitMQ e provedor devem ter timeout ou deadline.
- Erros devem preservar a causa e adicionar contexto útil, sem expor secrets.
- Valores monetários usam inteiros na menor unidade da moeda, nunca ponto
  flutuante.
- Instantes são persistidos e transmitidos em UTC.
- Logs são estruturados com Zap e usam identificadores de correlação.
- `go.mod` e `go.sum` são versionados no repositório.

## Organização

```text
cmd/          pontos de entrada dos binários
internal/     domínio, casos de uso e adapters privados
api/          contrato OpenAPI
db/           migrations e queries
deployments/  configuração específica de ambientes
docs/         documentação e decisões
tests/        testes que atravessam mais de um pacote
```

Dependências devem apontar para dentro: domínio não conhece HTTP, PostgreSQL,
RabbitMQ ou SDKs de provedores. Adapters implementam as interfaces definidas
pelos casos de uso.

## API HTTP

- `api/openapi.yaml` é a fonte de verdade do contrato HTTP.
- O strict server e os tipos gerados por `oapi-codegen` não são editados
  manualmente.
- Toda rota pública deve estar no OpenAPI com exemplos de sucesso e erro.
- Alterações no contrato atualizam implementação, testes e documentação no
  mesmo pull request.
- Respostas de erro seguem uma estrutura comum com código estável, mensagem e
  identificador de correlação.
- Novos campos de resposta devem ser opcionais quando isso preservar
  compatibilidade.
- Campos não são removidos, renomeados ou reinterpretados silenciosamente.
- Identificadores públicos são opacos e não sequenciais.
- Operações repetíveis exigem `Idempotency-Key`.
- Rotas da primeira geração usam o prefixo `/v1`.

## PostgreSQL

- Migrations publicadas são imutáveis; correções usam uma nova migration.
- Goose é a única autoridade de migrations; `AutoMigrate` não é usado.
- Nomes de tabelas, colunas e constraints usam `snake_case`.
- Constraints do banco protegem unicidade, idempotência e referências.
- Mudanças destrutivas seguem a sequência `expand`, `migrate`, `contract`.
- Queries cuja forma fizer parte da garantia devem ser explícitas e gerar tipos
  por meio do `sqlc`.
- GORM coordena o CRUD e as transações curtas de pedido e pagamento, enquanto
  constraints e índices do schema garantem idempotência e concorrência. A
  gravação de inbox e outbox, a deduplicação de eventos, o lease do relay e as
  transições condicionais do consumer usam `sqlc`: SQL explícito sempre que a
  forma da query fizer parte da garantia. A fronteira entre os dois estilos é
  por query, e não por tabela.
- Models do GORM não são entidades do domínio e hooks não contêm regras de
  negócio.
- O schema do banco é interno e não deve ser consumido diretamente por
  integradores.
- Transações devem ser curtas e não manter locks enquanto chamam serviços
  externos.

## RabbitMQ

- Exchanges, filas e mensagens do fluxo financeiro são duráveis.
- Publicações usam publisher confirms e partem do transactional outbox.
- Consumers usam ack manual somente depois que os efeitos forem persistidos.
- Consumers são idempotentes e aceitam redelivery.
- Concorrência e prefetch são explícitos e configuráveis.
- Falhas transitórias usam retry com backoff e limite de tentativas.
- Falhas definitivas seguem para uma dead-letter queue.
- Não é permitido criar loop infinito com `nack` e requeue imediato.
- Mensagens possuem `messageId`, `type`, `schemaVersion`, `occurredAt`,
  `correlationId` e payload mínimo.
- Mudanças incompatíveis criam nova versão do schema ou nova routing key.
- Mensagens não contêm PAN, CVV, secrets ou payloads desnecessários do provedor.

## Logs, métricas e traces

- Logs devem ser estruturados e conter nível, mensagem e correlação.
- Produção usa logs JSON; desenvolvimento pode usar formato de console.
- Traces e métricas usam OpenTelemetry e exportação OTLP.
- Secrets, credenciais e instrumentos de pagamento nunca aparecem em logs.
- IDs de negócio podem aparecer em logs controlados e traces, mas não como
  labels de métricas.
- Métricas seguem as definições de [métricas prioritárias](metrics.md).
- Chamadas externas propagam contexto de trace quando o protocolo permitir.

## Testes

- Casos de uso têm testes unitários para invariantes e transições.
- Adapters têm testes de integração com dependências reais em containers quando
  o comportamento da dependência for relevante.
- Mudanças de API validam request, response e contrato OpenAPI.
- Fluxos assíncronos testam duplicação, redelivery, timeout, retry e DLQ.
- Correções de bugs incluem um teste que falharia antes da correção.
- Percentual de cobertura não substitui a cobertura dos cenários críticos.
- Testes em `tests/e2e` usam a build tag `e2e`, executam serialmente e não usam
  `t.Parallel()`, pois compartilham a topologia RabbitMQ de nomes constantes.
- Cleanup destrutivo em testes exige uma flag de consentimento explícita,
  validação de alvo local e allowlists de tabelas e filas. Nunca se trunca ou
  purga a partir de nomes, URLs ou globs fornecidos livremente.
- Esperas por estado assíncrono usam polling com deadline e diagnóstico do
  último estado observado; `time.Sleep` fixo não é mecanismo de sincronização.
- A suíte e2e e os testes de adapter RabbitMQ não executam simultaneamente
  quando apontam para a mesma instalação e as mesmas filas.
- Hooks de falha são instalados apenas por opções explícitas de teste; não há
  variável de ambiente de produção que ative nack, interrupção ou crash.
- Chaos curto e determinístico roda em todo pull request. Recuperação real e
  matriz de backlog usam a tag `chaos`, dependências exclusivas e execução
  nightly/manual, sendo repetidas no mesmo SHA antes de uma release.

## Git e pull requests

- `main` é a branch principal e deve permanecer publicável.
- Mudanças são feitas em branches curtas e integradas por pull request.
- O método preferido é squash merge.
- O título do pull request segue Conventional Commits e se torna a mensagem do
  commit consolidado.
- O CI obrigatório deve passar antes do merge.
- Toda entrega revisa e atualiza a documentação no mesmo pull request.
- Itens documentais não aplicáveis são justificados no checklist do pull
  request; a revisão não é omitida.
- Roadmap, estado no README e changelog são atualizados quando a entrega ou fase
  correspondente for concluída.

A definição de pronto está detalhada no
[fluxo de encerramento de entregas](delivery-workflow.md).

Tipos adotados:

```text
feat       nova funcionalidade
fix        correção de comportamento
docs       documentação
test       testes
refactor   alteração interna sem mudar comportamento
perf       melhoria de desempenho
build      build ou dependências
ci         automação de integração
chore      manutenção que não se encaixa nas anteriores
```

Exemplos:

```text
feat(api): add checkout creation
fix(worker): prevent duplicate payment transition
docs(rabbitmq): explain retry topology
test(outbox): cover broker unavailability
```

Breaking changes usam `!` e explicação no corpo:

```text
feat(api)!: rename checkout response

BREAKING CHANGE: checkoutUrl replaces redirectUrl.
```

Referência: [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/).
