# Payments Boilerplate

API boilerplate open source para integrar pagamentos de forma segura, previsível
e reutilizável.

O projeto será uma API headless escrita em Go que orquestra a Stripe por meio
do Checkout hospedado. Sua interface de demonstração será uma documentação
OpenAPI com Swagger UI. Eventos serão
processados de forma assíncrona e durável com PostgreSQL e RabbitMQ. O projeto
não será um gateway, uma instituição financeira nem um sistema que captura ou
armazena dados completos de cartão.

> [!IMPORTANT]
> O projeto está na fase de fundação executável. Ainda não existe uma versão pronta para
> produção e o uso deste código não garante conformidade com PCI DSS, LGPD ou
> qualquer outra obrigação regulatória.

## Desenvolvimento local

Pré-requisitos:

- Go 1.26 ou 1.27;
- Docker com Docker Compose;
- Make, opcional, para os atalhos documentados.

Prepare a configuração e as dependências:

```bash
cp .env.example .env
make infra-up
make migrate-up
make generate
```

Execute API e worker em terminais separados:

```bash
make run-api
make run-worker
```

A API fica disponível em `http://localhost:8080`, a documentação em
`http://localhost:8080/docs/` e o painel local do RabbitMQ em
`http://localhost:15672`.

Valide a API sem abrir o navegador:

```bash
curl --fail --show-error http://localhost:8080/health
curl --fail --show-error http://localhost:8080/openapi.yaml
```

Para incluir o ambiente de observabilidade:

```bash
docker compose --profile observability up -d
```

O Grafana fica disponível em `http://localhost:3000`. Consulte a
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

## Escopo inicial

A primeira entrega será deliberadamente pequena:

- Pagamento único para comércio eletrônico.
- Uma moeda inicial: BRL.
- Um único provedor: Stripe.
- Stripe Checkout hospedado, inicialmente para cartão em BRL.
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

## Jornada da pessoa desenvolvedora

A pessoa usuária deste projeto é quem desenvolve o sistema que venderá o produto
ou serviço. No MVP, o Swagger UI ocupa o lugar do sistema que futuramente
consumirá a API.

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
| Acesso ao banco | `pgx` + GORM para CRUD + `sqlc` para SQL crítico |
| Mensageria | RabbitMQ |
| Confiabilidade de publicação | Transactional outbox |
| Provedor | Stripe Checkout hospedado |
| Logs | Zap |
| Métricas e traces | OpenTelemetry |
| Empacotamento local | Docker Compose |

## Documentação

- [Visão e escopo](docs/product-vision.md)
- [Arquitetura inicial](docs/architecture.md)
- [Contrato inicial da API](docs/api.md)
- [Integração com Stripe](docs/providers/stripe.md)
- [Convenções do projeto](docs/conventions.md)
- [Versionamento](docs/versioning.md)
- [Métricas prioritárias](docs/metrics.md)
- [Roadmap](docs/roadmap.md)
- [Estrutura e responsabilidades dos diretórios](docs/project-structure.md)
- [Deploy na Railway](docs/deployment/railway.md)
- [Decisão arquitetural: limites do produto](docs/decisions/0001-project-boundaries.md)
- [Decisão arquitetural: API headless](docs/decisions/0002-headless-api.md)
- [Decisão arquitetural: Go e RabbitMQ](docs/decisions/0003-go-rabbitmq.md)
- [Decisão arquitetural: licença](docs/decisions/0004-apache-license.md)
- [Decisão arquitetural: convenções, versionamento e suporte](docs/decisions/0005-conventions-versioning-support.md)
- [Decisão arquitetural: Stripe como primeiro provedor](docs/decisions/0006-stripe-first-provider.md)
- [Decisão arquitetural: GORM e sqlc](docs/decisions/0007-gorm-and-sqlc.md)
- [Decisão arquitetural: Zap e OpenTelemetry](docs/decisions/0008-observability-stack.md)
- [Decisão arquitetural: Railway](docs/decisions/0009-railway-deployment.md)
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
| Meio inicial | Cartão em BRL; Pix após validar o fluxo assíncrono |
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
