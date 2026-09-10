# Changelog

Todas as mudanças relevantes deste projeto serão documentadas neste arquivo.

O formato é inspirado em [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
e o projeto segue [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Visão, escopo, arquitetura e roadmap iniciais.
- Contrato conceitual da API headless.
- Métricas prioritárias do produto e da operação.
- Política de segurança e guia de contribuição.
- Apache License 2.0.
- Decisões arquiteturais para Go, PostgreSQL, RabbitMQ e transactional outbox.
- Convenções, política de versionamento e política de suporte.
- Stripe Checkout como primeiro provedor e plano de integração em sandbox.
- Esquema de `orders`, `payments`, `payment_attempts` e `webhook_events`.
- `POST /v1/orders` com valor e moeda calculados no servidor a partir de um
  catálogo fixo, header `Idempotency-Key` obrigatório e erros JSON com código
  estável e identificador de correlação.
- Modelo de acesso self-hosted e single-integrator, com autenticação por API key
  protegendo as rotas de negócio por padrão, rotação de credenciais e health
  público.
- Stripe Checkout hospedado para cartão e Pix em BRL, com sessão ligada a
  `Payment` e `PaymentAttempt`, proteção contra checkouts concorrentes e
  persistência da URL antes da resposta, validado por um pagamento completo no
  sandbox.
- Inspeção autenticada de metadados da inbox/outbox e replay atômico de trabalho
  com falha, reutilizando a mensagem original e mantendo contador de replay.
- Validação automatizada do fluxo Pix de `processing` até `paid` após o evento
  `checkout.session.async_payment_succeeded`.
- Idempotência transparente em criação de pedidos e checkouts, além de
  `GET /v1/orders/{orderId}` para consultar o estado local.
- `POST /v1/webhooks/stripe`, público na rede e autenticado pela verificação da
  assinatura sobre os bytes brutos da requisição, com proteção contra replay
  pela janela de tolerância e limite de corpo próprio.
- Inbox e outbox gravadas na mesma transação, com deduplicação garantida pelo
  índice único do evento do provedor e mensagem que referencia o evento em vez
  de copiar o payload da Stripe. Testes de integração cobrem o rollback da
  inbox quando a outbox falha, entregas concorrentes do mesmo evento e a
  unicidade imposta pelo próprio schema.
- Relay do outbox executando dentro do `worker`. Ele reserva lotes por lease em
  transações curtas, publica sem manter transação aberta durante a chamada ao
  broker e só marca uma mensagem como publicada após o publisher confirm. Mais
  de uma instância de worker é segura sem coordenação externa, e um relay que
  morre devolve seu trabalho quando o lease expira.
- Publicação `mandatory`, para que uma mensagem que nenhuma fila recebeu seja
  reportada em vez de contar como publicada apesar do confirm.
- Política de falhas do relay: indisponibilidade do broker, timeout, confirm
  negativo e mensagem não roteada são transitórios e reagendados com backoff
  exponencial e jitter, sem nunca descartar a mensagem. Apenas erros
  classificados como permanentes abandonam uma mensagem, e o limite de
  tentativas apenas dispara alerta.
- Reconexão AMQP sob demanda, com prazo de socket cobrindo rediscagem,
  handshake, RPCs de preparação, envio e espera do confirm. O driver não
  interrompe todas essas operações apenas com o cancelamento do `context`, por
  isso o limite permanece instalado no transporte durante a tentativa inteira.
  O worker também passa a subir com o broker indisponível, já que as mensagens
  estão duráveis no PostgreSQL.
- Prazos do lease calculados pelo PostgreSQL, que é o único relógio
  compartilhado entre instâncias, e dono do lease identificando o processo e
  não a máquina, para que dois workers no mesmo host não colidam. Uma janela
  monotônica local, iniciada antes do lease, limita a publicação sem comparar o
  relógio absoluto do banco com o relógio do worker e reserva tempo para a
  liquidação.
- Perda de lease detectada e reportada em vez de contabilizada como sucesso,
  já que a mensagem será publicada novamente por outra instância.
- Topologia do RabbitMQ declarada de forma idempotente pela aplicação, com
  exchange `payments.events`, fila durável `payments.webhooks` e dead-letter
  criada desde o início, já que argumentos de fila são imutáveis.
- Consumer executando dentro do `worker`, ao lado do relay, com ack manual,
  prefetch explícito, concorrência limitada e conexões AMQP próprias. Uma
  publicação do relay não pode mais interferir no canal de consumo.
- Aplicação dos efeitos de um evento em uma única transação `sqlc`, que bloqueia
  o agregado inteiro — evento, tentativa, pagamento e pedido — em ordem fixa.
  Eventos diferentes do mesmo pagamento passam a ser serializados entre si, e
  não apenas as entregas repetidas de um mesmo evento.
- Matriz de transições aplicada contra o estado bloqueado antes de qualquer
  escrita, distinguindo efeito aplicado, no-op deliberado e inconsistência
  terminal. Um evento negativo depois de um estado final positivo é ignorado
  como reordenação; um evento positivo sobre um estado final contraditório vai
  para a dead-letter em vez de ser silenciosamente descartado. Uma transição
  aprovada que não altere exatamente uma linha é reportada como invariante
  violada.
- Interpretação do payload armazenado pelo adapter da Stripe, com verificação de
  compatibilidade da versão de API — o débito que a recepção deixou explícito —
  e conferência de valor, moeda e correlação entre os identificadores antes de
  aplicar qualquer efeito.
- `checkout.session.completed` sem pagamento liquidado passa a produzir
  `processing` em vez de concluir o pedido, que é o caminho do Pix e de todo
  meio de confirmação tardia.
- Retry com backoff por faixas: uma exchange fanout e uma fila quorum por faixa,
  com TTL próprio e dead-lettering de volta à `payments.events`. Uma fila por
  faixa evita que uma espera longa bloqueie as curtas na cabeça da fila, e o
  fanout preserva a routing key original no caminho de volta. A contagem de
  tentativas vem da inbox, que é durável, e não de headers da mensagem.
- Republicação explícita e confirmada para retry e para a dead-letter, com
  `mandatory` e publisher confirm antes do ack da mensagem original. O
  dead-lettering de fila clássica republica sem confirms e pode perder a
  mensagem a caminho da DLQ; por isso até mensagens malformadas seguem a
  republicação explícita.
- Encerramento deliberado da sessão de consumo quando aplicar, registrar uma
  falha, republicar ou dar ack falha, liberando mensagens sem ack para
  redelivery após o backoff em vez de ocupar o prefetch indefinidamente.
- Fechamento atômico da inbox quando o retry budget se esgota, validação de
  provider e proteção dos estados `partially_refunded` e `refunded` contra
  eventos tardios de checkout.
- Decisão de transação, transições e política de falha do consumer, registrada
  no ADR 0013.
- Decisão de execução do relay e da topologia do broker, registrada no ADR 0012.
- Decisão de recepção de webhooks da Fase 3: contrato de resposta por
  situação, que define quando o provedor deve reentregar, e mensagem de
  outbox por referência ao evento, sem copiar o payload do provedor.
- Classificação dos dados persistidos, política de retenção e modelo de ameaça,
  registrados no ADR 0017 e em `docs/security.md`. As duas cópias do payload da
  Stripe em `webhook_events` são declaradas como dados pessoais, cada uma com
  finalidade, acesso e prazo de retenção escritos, e `last_error` passa a ser
  tratado como superfície pública por ser devolvido pela API operacional.
- Procedimento operacional de expurgo com o SQL correspondente. A `0.1.0` não
  terá expurgo automatizado, e a ausência de automação passa a ser decisão
  registrada em vez de retenção indefinida silenciosa.
- Verificação executável do contrato OpenAPI contra exchanges HTTP reais, com
  validação de request, status, headers, media type, corpo, exemplos e um
  manifesto exato para todos os pares operação/status documentados.
- Harness E2E serial e isolado cobrindo autenticação, preço confiável,
  idempotência, checkout, recepção atômica e deduplicada de webhooks, cartão,
  Pix, eventos fora de ordem, DLQ, redelivery, concorrência e reprocessamento.
- Hooks de teste para as janelas entre publish/confirm/settlement/commit/ack e
  proxy TCP para indisponibilidade, blackhole e recuperação de PostgreSQL e
  RabbitMQ, com matriz prolongada de backlog e percentis de drenagem.
- Jobs dedicados para contrato, E2E e chaos determinístico; chaos prolongado
  roda por agendamento ou manualmente e bloqueia releases no mesmo SHA da tag.

### Changed

- Documentação sincronizada com o estado real da Fase 4: roadmap e README agora
  registram rate limiting, revisão de dados sensíveis e fluxo de contribuição
  como concluídos; arquitetura, integração Stripe e estrutura de testes deixam
  de apresentar os testes de falha já entregues como trabalho futuro.
- Superfície HTTP fechada. A documentação passa a ser opt-in por `DOCS_ENABLED`,
  com default `false` em todos os ambientes: sem o opt-in, `/docs`, `/docs/` e
  `/openapi.yaml` respondem `404` como qualquer caminho inexistente. Em
  `APP_ENV=development` o opt-in serve a documentação sem credencial; em
  qualquer outro ambiente ele exige `X-API-Key` válida nas três rotas, decidida
  antes do redirect e da verificação de método, e o startup registra um warning
  que nomeia o ambiente e nunca a chave. `.env.example` e o Compose de
  desenvolvimento ligam a documentação explicitamente. Toda resposta, incluindo
  `404` e as do recovery, passa a carregar `X-Content-Type-Options`,
  `Referrer-Policy`, `Cache-Control: no-store`, `X-Frame-Options` e
  `Content-Security-Policy`; a página do Swagger recebe uma política própria com
  os scripts inline liberados por hash, validada contra os recursos que o
  `swgui` realmente serve. CORS permanece desligado. O worker deixa de registrar
  o strict server completo e publica somente `GET /health`; as demais operações
  do contrato respondem `404` naquele processo, em vez de `401` ou `501`.
  `TRUSTED_PROXY_CIDRS` define de quais peers `X-Forwarded-For` é acreditado,
  com parsing e validação no startup, e a resolução do endereço do cliente
  ficou centralizada para que o rate limiting a reutilize. Registrado no
  [ADR 0016](docs/decisions/0016-http-surface-and-client-identity.md).
- Métricas da aplicação para outbox, filas, retries e DLQ, concentradas em
  `internal/platform/metrics`. O pacote implementa as interfaces de observer que
  as camadas já declaravam, então domínio e casos de uso continuam sem importar
  OpenTelemetry; onde faltava informação para uma métrica honesta, a interface
  original foi estendida por uma segunda interface opcional que só o observer de
  métricas implementa, e `WithObserver` passou a aceitar logging e métricas lado
  a lado. Contadores e histogramas cobrem recepção de webhook, assinatura
  inválida, duplicação, chamadas ao provedor por operação e desfecho, replay e
  conflito de idempotência, transições de estado, e o ciclo completo de relay e
  consumer, incluindo mensagem presa, falha permanente, lease perdido,
  redelivery, retry, no-op e DLQ.
- Gauges de estado alimentados por samplers em background, com timeout por
  rodada e publicação do último valor conhecido: outbox pendente e idade do mais
  antigo, inbox pendente, com falha e idade do mais antigo, profundidade de cada
  fila declarada e uso do pool PostgreSQL por estado. O sampler de fila usa
  conexão e canal próprios, porque uma declaração passiva de fila inexistente
  fecha o canal em que roda e o timeout instalaria um prazo no socket do
  consumo. Uma coleta que falha não zera nem apaga série: os gauges seguram o
  último valor, `metrics.sampler.failures` incrementa e `metrics.sampler.age`
  cresce, de modo que banco ou broker indisponível vira sinal visível em vez de
  coleta bloqueada ou processo derrubado.
- Allowlist explícita de labels, com normalização para `other`. Identificador,
  chave de idempotência, `correlation_id`, segredo, texto de mensagem de erro,
  caminho HTTP livre e tipo de evento não normalizado não podem virar label, e
  testes falham quando uma instrumentação tenta. Nomes de fila são limitados à
  topologia que o próprio processo declarou. A lista não é derivada dos enums do
  domínio: um provedor ou estado novo é decisão registrada, não série que
  aparece sozinha.
- Dashboard Grafana versionado em `deployments/observability`, cobrindo HTTP,
  provedor, inbox, outbox, retry, DLQ e pool do PostgreSQL, provisionado somente
  leitura pelo profile `observability` do Compose. Um teste verifica que toda
  consulta nomeia instrumento publicado e apenas labels permitidas.
- `METRICS_SAMPLE_INTERVAL` e `METRICS_SAMPLE_TIMEOUT` configuram a amostragem.
  A configuração recusa timeout maior ou igual ao intervalo, que transformaria
  uma dependência travada em amostragem concorrente ilimitada em vez de uma
  lacuna visível. Desabilitar OTLP não muda comportamento funcional: os
  instrumentos são criados contra o provider no-op e nada é registrado.
- `docs/metrics.md` reescrito para separar o que a aplicação publica hoje das
  referências operacionais e metas de produto, que a lista anterior de vinte e
  um nomes misturava. Decisão registrada no
  [ADR 0015](docs/decisions/0015-application-metrics-and-cardinality.md).

- `X-Correlation-ID` passa a fazer parte obrigatória das 33 respostas do
  OpenAPI, com código gerado e handlers alinhados ao contrato.
- Canal privado de relato de vulnerabilidades habilitado no repositório. O
  `SECURITY.md` deixa de instruir a abertura de issue pública provisória e
  aponta para o formulário privado do GitHub.
- README e `SECURITY.md` deixam de sugerir ausência de dados pessoais. A
  ausência de dado completo de cartão continua verdadeira e passa a ser
  apresentada apenas como o que é: consequência do Checkout hospedado.

- Composição executável da API e do worker extraída para `internal/runtime/api`
  e `internal/runtime/worker`, pacotes importáveis que recebem `context.Context`,
  `config.Config` e dependências opcionais tipadas pelas portas da aplicação.
  `cmd/api` e `cmd/worker` passam a responder apenas por configuração, sinais e
  código de saída, e binários e testes passam a usar a mesma composição. O
  servidor HTTP aceita um listener já aberto, o que elimina a corrida de escolher
  uma porta livre e depois tentar abri-la de novo. A substituição de uma porta
  Stripe dispensa somente a configuração daquele adapter, preservando a
  validação do checkout ou webhook real que continuar ativo. O shutdown aguarda
  a goroutine do servidor e força o fechamento depois do timeout. Registrado no
  [ADR 0014](docs/decisions/0014-testable-composition-and-e2e-boundaries.md).
- Documentação sincronizada com o fim da Fase 2, distinguindo o checkout já
  executável do pipeline assíncrono planejado para a Fase 3 e resumindo o estado
  do roadmap no README.
- Definição de pronto documental e template de pull request adicionados para
  manter documentação, roadmap e changelog alinhados a cada entrega.

[Unreleased]: https://github.com/rmotti/payments-boilerplate/commits/main
