# Payments Boilerplate

API boilerplate open source para integrar pagamentos de forma segura, previsível
e reutilizável.

O projeto é uma API headless escrita em Go que já cria pedidos com preço
calculado no servidor e abre Stripe Checkout hospedado em sandbox. O contrato
OpenAPI e o Swagger UI formam sua interface de demonstração. A confirmação por
webhook e o processamento assíncrono durável com PostgreSQL e RabbitMQ estão
implementados, incluindo inspeção, replay seguro e o caminho assíncrono do Pix.
O projeto não é um gateway, uma instituição financeira
nem um sistema que captura ou armazena dados completos de cartão.

> [!IMPORTANT]
> As Fases 0 a 4 do roadmap inicial estão concluídas, mas a versão `0.1.0`
> ainda depende da validação final da CI e da publicação da release. O uso
> deste código não garante conformidade com PCI DSS, LGPD ou qualquer outra
> obrigação regulatória.

> [!WARNING]
> A aplicação **armazena dados pessoais**. Cada evento da Stripe é gravado na
> tabela `webhook_events`, e um evento de Checkout carrega nome, e-mail,
> telefone e endereços do cliente. Não armazenar dado completo de cartão não
> torna o restante do payload inofensivo. O inventário, os prazos de retenção e
> o procedimento de expurgo estão em [docs/security.md](docs/security.md).

## Estado atual e roadmap

Este quadro é apenas um resumo. O checklist completo e sua ordem de execução
ficam em [docs/roadmap.md](docs/roadmap.md), que é a fonte de verdade.

| Etapa | Estado | Resultado principal |
| --- | --- | --- |
| Fase 0 — Definições de fundação | Concluída | Visão, limites, decisões técnicas e convenções |
| Fase 1 — Fundação executável | Concluída | API, banco, migrations, observabilidade, CI e deploy documentado |
| Fase 2 — Primeiro pagamento vertical | Concluída | API key, pedido idempotente, consulta e Stripe Checkout em BRL |
| Fase 3 — Confirmação assíncrona confiável | Concluída | Webhook, inbox/outbox, relay, consumer, inspeção, replay seguro e Pix |
| Fase 4 — Qualidade para publicação | Concluída | Hardening, testes de falha, métricas, sandbox, checklist operacional e revisão de segurança |

Um pagamento concluído na Stripe agora chega ao estado local: o webhook
verificado é gravado na inbox, publicado pelo relay e aplicado pelo consumer,
que move tentativa, pagamento e pedido em uma única transação. Nenhuma URL de
retorno altera estado.

## Desenvolvimento local

Pré-requisitos:

- Go 1.26 ou 1.27;
- Docker com Docker Compose;
- Make, opcional, para os atalhos documentados;
- uma conta Stripe com chave secreta de teste (`sk_test_...`) para abrir o
  checkout real.

Prepare a configuração e as dependências:

```bash
cp .env.example .env
openssl rand -hex 32
make infra-up
make migrate-up
make generate
```

Para uma jornada guiada que cria um Checkout real de teste, emite fixtures
assinadas de cartão/Pix, aguarda o pipeline e limpa apenas os dados locais
criados pelo próprio utilitário, consulte o
[guia de sandbox reproduzível](docs/sandbox.md).

Copie a saída do `openssl` para `INTEGRATION_API_KEYS` no `.env`. A variável
aceita uma ou mais chaves separadas por vírgula para permitir rotação. Cada
chave deve ter ao menos 32 caracteres; use o comando acima para gerar 256 bits
aleatórios em vez de inventar uma senha.

Copie também uma chave secreta do modo de teste da Stripe para
`STRIPE_SECRET_KEY`. As URLs de sucesso e cancelamento da `.env.example` voltam
à documentação local e podem ser substituídas pelas páginas do seu frontend.

Execute API e worker em terminais separados:

```bash
make run-api
make run-worker
```

O processo `worker` publica seu healthcheck e executa dois componentes: o relay
do outbox, que declara a topologia do RabbitMQ e publica as mensagens gravadas
pela API, e o consumer, que as aplica com ack manual, concorrência limitada,
retry com backoff e dead-letter queue.

A API fica disponível em `http://localhost:8080`, a documentação em
`http://localhost:8080/docs/` e o painel local do RabbitMQ em
`http://localhost:15672`.

A documentação só aparece porque a `.env.example` define `DOCS_ENABLED=true`. O
default da aplicação é `false` em todos os ambientes, e fora de
`APP_ENV=development` habilitá-la passa a exigir `X-API-Key` válida. Consulte o
[ADR 0016](docs/decisions/0016-http-surface-and-client-identity.md).

Valide a API sem abrir o navegador:

```bash
curl --fail --show-error http://localhost:8080/health
curl --fail --show-error http://localhost:8080/openapi.yaml
```

Escolha uma das chaves configuradas para os comandos seguintes:

```bash
export API_KEY='<a-mesma-chave-gravada-em-INTEGRATION_API_KEYS>'
```

Crie um pedido de demonstração. O valor vem do catálogo do servidor, nunca da
requisição (copie o campo `id` da resposta):

```bash
curl --fail --show-error http://localhost:8080/v1/orders \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"productId":"product_demo","quantity":1}'
```

Use esse identificador para criar o checkout:

```bash
export ORDER_ID='ord_...'
curl --fail --show-error -X POST \
  "http://localhost:8080/v1/orders/$ORDER_ID/checkout" \
  -H "X-API-Key: $API_KEY" \
  -H "Idempotency-Key: $(uuidgen)"
```

Abra a `checkoutUrl` retornada e escolha cartão ou Pix. Para cartão no sandbox,
use `4242 4242 4242 4242`, uma data futura e qualquer CVC. O Pix só aparece
para contas Stripe elegíveis e com o método habilitado. Consulte o estado local
com:

```bash
curl --fail --show-error \
  "http://localhost:8080/v1/orders/$ORDER_ID" \
  -H "X-API-Key: $API_KEY"
```

O pagamento é concluído na Stripe, o webhook registra o evento de forma durável,
o relay publica a mensagem no RabbitMQ e o consumer aplica a transição: o pedido
chega a `paid` e o pagamento a `succeeded`. A página de retorno nunca confirma
pagamento; somente o evento verificado altera estado.

Inspecione falhas da inbox/outbox sem expor o payload do provedor e reenfileire
uma delas de forma atômica:

```bash
curl --fail --show-error \
  "http://localhost:8080/v1/webhook-events?status=failed&limit=50" \
  -H "X-API-Key: $API_KEY"

export WEBHOOK_EVENT_ID='evt_...'
curl --fail --show-error -X POST \
  "http://localhost:8080/v1/webhook-events/$WEBHOOK_EVENT_ID/reprocess" \
  -H "X-API-Key: $API_KEY"
```

O replay aceita apenas trabalho que falhou, reutiliza a mensagem original e
registra a quantidade e o instante das solicitações de reprocessamento.

Para incluir o ambiente de observabilidade:

```bash
make observability-up
```

O Grafana fica disponível em `http://localhost:3000`, com o dashboard do
pipeline de pagamentos já provisionado: HTTP, provedor, inbox, outbox, retry,
DLQ e pool do PostgreSQL. Defina `OTEL_ENABLED=true` para que a aplicação
exporte. Os instrumentos publicados e as referências operacionais estão em
[métricas](docs/metrics.md). Consulte a
[estrutura do projeto](docs/project-structure.md) para entender as fronteiras
dos pacotes.

Os mesmos processos podem ser executados inteiramente em containers:

```bash
docker compose --profile app up --build
```

## Objetivos

- Reduzir o tempo necessário para integrar um primeiro pagamento.
- Demonstrar padrões seguros para checkout, webhooks e idempotência.
- Separar as regras de negócio dos detalhes do provedor de pagamentos.
- Oferecer uma API executável, contrato OpenAPI, testes e documentação
  suficientes para que outra pessoa consiga adaptar o projeto.
- Tornar falhas, retries e mudanças de estado explícitos e auditáveis.

## Escopo da versão 0.1

O objetivo da versão `0.1.0` é deliberadamente pequeno:

- Pagamento único para comércio eletrônico.
- Uma moeda inicial: BRL.
- Um único provedor: Stripe.
- Stripe Checkout hospedado para cartão e Pix em BRL.
- Confirmação do resultado por webhook verificado.
- Persistência de pedidos, pagamentos, tentativas e eventos.
- Proteção contra requisições e eventos duplicados.
- Transactional outbox para publicação confiável de mensagens.
- Processamento assíncrono com RabbitMQ e worker idempotente.
- Swagger UI, exemplos de requisição e ambiente de testes/sandbox.

Um frontend próprio, autenticação de consumidor e painel administrativo não
fazem parte do MVP.

Assinaturas, marketplaces, split, repasses e múltiplos provedores não fazem
parte da primeira versão.

Cada instalação atende um único integrador e usa seu próprio banco, conta
Stripe e credenciais. O modelo aprovado exige `X-API-Key` nas rotas de negócio,
mantém health público e autentica webhooks pela assinatura da Stripe. A API
aceita mais de uma chave ativa para rotação e responde `401 Unauthorized` sem
distinguir chave ausente de inválida.

A documentação é opt-in por `DOCS_ENABLED`, desligada por padrão em todos os
ambientes. Sem opt-in, `/docs`, `/docs/` e `/openapi.yaml` respondem `404` como
qualquer caminho inexistente; fora de `APP_ENV=development`, habilitá-los exige
a mesma `X-API-Key`. Toda resposta carrega headers de segurança e nenhuma emite
CORS. O worker publica somente `/health`. `X-Forwarded-For` só é acreditado
quando o peer pertence a `TRUSTED_PROXY_CIDRS`.

Toda operação é limitada por taxa, com defaults folgados e ligados por padrão.
São quatro limites: um grosseiro por endereço do cliente para negócio, operações
e documentação, aplicado antes do parsing e da autenticação; um por credencial
válida, chaveado por uma impressão HMAC e nunca pela chave em texto puro; um
balde global aplicado ao webhook antes da leitura do corpo, porque o endereço da
Stripe não é identidade confiável; e um balde próprio para o health, para que
tráfego comum não derrube a probe. O default grosseiro (1 200/min) é maior que o
de credencial (600/min), de modo que a quota do integrador seja efetiva. Uma recusa devolve `429` com o
envelope de erro comum, `Retry-After` e o código `rate_limited`, sem revelar
qual limite foi atingido. Os baldes vivem em memória com capacidade e TTL
limitados, e os limites são por processo: com N réplicas, o teto efetivo é N
vezes o configurado. Ajuste pelas variáveis `RATE_LIMIT_*` ou desligue com
`RATE_LIMIT_ENABLED=false` se já limitar na borda; a política completa está no
[ADR 0018](docs/decisions/0018-rate-limiting.md).

## Jornada da pessoa desenvolvedora

A pessoa usuária deste projeto é quem desenvolve o sistema que venderá o produto
ou serviço. No MVP, o Swagger UI ocupa o lugar do sistema que futuramente
consumirá a API.

O diagrama abaixo representa o fluxo implementado na versão `0.1.0`.

```mermaid
sequenceDiagram
    autonumber
    actor Dev as Pessoa desenvolvedora
    participant Docs as Swagger UI
    participant API as Payments API
    participant DB as PostgreSQL
    participant MQ as RabbitMQ
    participant Worker as Payments Worker
    participant PSP as Stripe Checkout (sandbox)

    Dev->>API: Configura credenciais e inicia o projeto
    Dev->>Docs: Abre a documentação OpenAPI
    Dev->>Docs: Solicita a criação de um pedido
    Docs->>API: POST /v1/orders
    API->>DB: Persiste pedido pending
    API-->>Docs: Retorna orderId, valor e moeda
    Dev->>Docs: Solicita o checkout do pedido
    Docs->>API: POST /v1/orders/{orderId}/checkout
    API->>PSP: Cria checkout com idempotência
    PSP-->>API: Retorna checkoutUrl
    API->>DB: Persiste a referência externa
    API-->>Docs: Retorna checkoutUrl
    Dev->>PSP: Abre a URL e paga em sandbox
    PSP->>API: Envia webhook assinado
    API->>DB: Persiste inbox e outbox na mesma transação
    API-->>PSP: Confirma o recebimento
    Worker->>DB: Busca mensagem pendente no outbox
    Worker->>MQ: Publica mensagem persistente
    MQ-->>Worker: Confirma a publicação
    Worker->>DB: Marca mensagem como publicada
    MQ->>Worker: Entrega mensagem
    Worker->>DB: Atualiza pagamento de forma idempotente
    Worker-->>MQ: Confirma o processamento
    Dev->>Docs: Consulta o pedido
    Docs->>API: GET /v1/orders/{orderId}
    API-->>Docs: Retorna status paid
```

Em uma integração real, o sistema da pessoa desenvolvedora fará as chamadas que
o Swagger UI representa no diagrama.

## Princípios

1. A aplicação é dona do pedido; o provedor executa o pagamento.
2. O navegador não é uma fonte confiável para confirmar o pagamento.
3. Toda operação monetária que possa ser repetida deve ser idempotente.
4. Webhooks podem ser duplicados, atrasados e entregues fora de ordem.
5. Valores monetários são inteiros na menor unidade da moeda, nunca `float`.
6. Nenhum endpoint próprio recebe PAN, CVV ou dados completos de cartão.
7. Secrets e dados sensíveis nunca são enviados ao cliente ou gravados em logs.
   O payload do provedor é dado pessoal, fica somente na inbox e está sujeito a
   retenção e expurgo.
8. Segurança e observabilidade fazem parte do produto, não são complementos.
9. Mensagens podem ser entregues mais de uma vez; seus efeitos são idempotentes.
10. Nenhuma publicação depende de uma escrita simultânea e não transacional no
    banco e no broker.

## Tecnologias do MVP

| Responsabilidade | Escolha |
| --- | --- |
| Linguagem | Go |
| Servidor HTTP | `net/http` |
| Contrato da API | OpenAPI contract-first |
| Geração de código | `oapi-codegen` strict server |
| Banco de dados | PostgreSQL |
| Acesso ao banco | `pgx` + GORM; `sqlc` reservado para SQL crítico explícito |
| Mensageria | RabbitMQ |
| Confiabilidade de publicação | Transactional outbox |
| Provedor | Stripe Checkout hospedado |
| Logs | Zap |
| Métricas e traces | OpenTelemetry |
| Empacotamento local | Docker Compose |

## Documentação

- [Visão e escopo](docs/product-vision.md)
- [Arquitetura](docs/architecture.md)
- [Contrato da API](docs/api.md)
- [Integração com Stripe](docs/providers/stripe.md)
- [Sandbox reproduzível](docs/sandbox.md)
- [Convenções do projeto](docs/conventions.md)
- [Fluxo de encerramento de entregas](docs/delivery-workflow.md)
- [Versionamento](docs/versioning.md)
- [Métricas](docs/metrics.md)
- [Roadmap](docs/roadmap.md)
- [Estrutura e responsabilidades dos diretórios](docs/project-structure.md)
- [Deploy na Railway](docs/deployment/railway.md)
- [Checklist operacional de implantação](docs/deployment/operations.md)
- [Revisão de segurança pré-0.1.0](docs/security-review-0.1.0.md)
- [Decisão arquitetural: limites do produto](docs/decisions/0001-project-boundaries.md)
- [Decisão arquitetural: API headless](docs/decisions/0002-headless-api.md)
- [Decisão arquitetural: Go e RabbitMQ](docs/decisions/0003-go-rabbitmq.md)
- [Decisão arquitetural: licença](docs/decisions/0004-apache-license.md)
- [Decisão arquitetural: convenções, versionamento e suporte](docs/decisions/0005-conventions-versioning-support.md)
- [Decisão arquitetural: Stripe como primeiro provedor](docs/decisions/0006-stripe-first-provider.md)
- [Decisão arquitetural: GORM e sqlc](docs/decisions/0007-gorm-and-sqlc.md)
- [Decisão arquitetural: Zap e OpenTelemetry](docs/decisions/0008-observability-stack.md)
- [Decisão arquitetural: Railway](docs/decisions/0009-railway-deployment.md)
- [Decisão arquitetural: acesso às rotas](docs/decisions/0010-route-access-model.md)
- [Decisão arquitetural: recepção de webhooks e conteúdo da outbox](docs/decisions/0011-webhook-reception-and-outbox.md)
- [Decisão arquitetural: relay do outbox e topologia](docs/decisions/0012-outbox-relay-and-topology.md)
- [Decisão arquitetural: transação, transições e falha do consumer](docs/decisions/0013-consumer-transactions-transitions-and-retry.md)
- [Decisão arquitetural: métricas da aplicação, cardinalidade e coleta de estado](docs/decisions/0015-application-metrics-and-cardinality.md)
- [Decisão arquitetural: superfície HTTP, documentação e identidade do cliente](docs/decisions/0016-http-surface-and-client-identity.md)
- [Decisão arquitetural: dados sensíveis, retenção e superfície de erro](docs/decisions/0017-sensitive-data-and-error-handling.md)
- [Dados, retenção e modelo de ameaça](docs/security.md)
- [Guia de contribuição](CONTRIBUTING.md)
- [Política de suporte](SUPPORT.md)
- [Changelog](CHANGELOG.md)
- [Política de segurança](SECURITY.md)

## Estado das decisões

| Tema | Estado |
| --- | --- |
| API de referência, não processador | Decidido |
| Produto headless com OpenAPI/Swagger UI | Decidido |
| Frontend próprio no MVP | Não terá |
| Linguagem | Go |
| Processamento assíncrono | RabbitMQ desde a versão 0.1 |
| Persistência e outbox | PostgreSQL |
| Primeiro fluxo: pagamento único | Decidido |
| Moeda inicial: BRL | Decidido |
| Checkout hospedado | Decidido |
| Primeiro provedor | Stripe Checkout |
| Meio inicial | Cartão e Pix em BRL |
| Modelo de implantação | Um integrador por instalação |
| Acesso às rotas de negócio | API key do integrador |
| Licença open source | Apache-2.0 |
| Versionamento | SemVer; série `v0.x` experimental |
| Commits e pull requests | Conventional Commits + squash merge |
| Suporte durante `v0.x` | Minor mais recente, melhor esforço, sem SLA |
| Público principal | Pessoas desenvolvedoras e equipes de software |

## Versionamento e suporte

A primeira entrega funcional será `v0.1.0`. Durante a série `v0.x`, versões
minor podem conter mudanças incompatíveis e somente a minor mais recente recebe
correções em melhor esforço. Consulte as políticas completas de
[versionamento](docs/versioning.md) e [suporte](SUPPORT.md).

## Licença

Código, documentação e exemplos são distribuídos sob a
[Apache License 2.0](LICENSE). Ela permite uso, modificação e distribuição,
inclusive comercial, de acordo com suas condições. O software é fornecido sem
garantias; consulte o texto completo da licença.

## Referências de segurança

- [PCI Security Standards Council — PCI DSS](https://www.pcisecuritystandards.org/standards/pci-dss/)
- [OWASP Application Security Verification Standard](https://owasp.org/www-project-application-security-verification-standard/)
- [ANPD — materiais educativos e publicações](https://www.gov.br/anpd/pt-br/centrais-de-conteudo/materiais-educativos-e-publicacoes)
