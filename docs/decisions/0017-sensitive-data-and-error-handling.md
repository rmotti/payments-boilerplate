# ADR 0017: dados sensíveis persistidos, retenção e superfície de erro

- Status: aceito
- Data: 2026-09-07
- Implementação: concluída para o escopo de E8b (sanitização de `errorText`,
  da leitura operacional, dos logs e da saída fatal dos processos; testes
  negativos com sentinelas; invariante testada
  sobre `raw_payload`/`payload`, e procedimento de rotação de credenciais).
  Guia operacional de expurgo, expurgo automatizado e canal privado de
  vulnerabilidades permanecem pendentes em E9/E11/pós-`0.1.0` — ver "Estado de
  implementação" abaixo e o backlog oficial em `docs/security.md`.

## Contexto

A Fase 4 precisa fechar o item 8 do roadmap antes das mudanças de código, porque
decisões tardias de privacidade obrigariam a refazer harness, métricas e
runbooks já construídos sobre premissas erradas.

A premissa errada existe hoje. A documentação do projeto trata a ausência de
dados completos de cartão como se fosse ausência de dados pessoais. Não é. O
`webhook_events` guarda **duas** cópias de cada evento da Stripe, em
`00002_payments.sql:124-136`:

- `raw_payload BYTEA`: o corpo exato cuja assinatura foi verificada durante a
  requisição, preservado porque `jsonb` reordena chaves e descarta formatação;
  como o header `Stripe-Signature` não é persistido, esses bytes sozinhos não
  permitem reverificação posterior nem constituem prova criptográfica
  independente;
- `payload JSONB`: o mesmo evento já parseado, para consulta e reprocessamento.

Um `checkout.session.completed` real carrega `customer_details` com nome,
e-mail, telefone, endereço de cobrança e endereço de entrega, além de
identificadores do provedor e dos metadados enviados pela integração. Ou seja: a
inbox é um repositório de dados pessoais, e as duas cópias precisam de
finalidade, acesso e retenção escritos.

Há uma segunda superfície, menos óbvia e mais perigosa. O campo `last_error`
existe em `webhook_events` e em `outbox_events`, é preenchido a partir de
`err.Error()` pelo `errorText` de
`internal/adapters/postgres/repositories/outbox.go:165-176` — função do pacote
`repositories`, usada tanto pelo caminho da outbox quanto pelo
`RecordFailure` de `internal/adapters/postgres/repositories/events.go:254` — e é
**devolvido pela API operacional** em `GET /v1/webhook-events` e
`GET /v1/webhook-events/{id}`. Truncar não é sanitizar: um erro de driver pode
carregar a DSN do PostgreSQL com senha, um erro de AMQP pode carregar a URL do
broker com credenciais, e um erro de validação sobre o evento pode carregar o
trecho do payload que o causou. O que hoje impede isso é apenas o conjunto de
erros que o código produz, não uma regra.

Uma coisa já está certa e precisa ser preservada como invariante, não como
acidente: as queries de `db/queries/operations.sql` selecionam colunas
explicitamente e **nunca** incluem `raw_payload` nem `payload`. O comentário no
topo do arquivo declara a intenção. Falta o teste que impede a regressão.

O desenho também já limita a exposição em um ponto importante. Por decisão do
[ADR 0011](0011-webhook-reception-and-outbox.md), a mensagem publicada no
RabbitMQ carrega apenas referência ao evento — `messageId`, `type`,
`schemaVersion`, `occurredAt`, `correlationId` e `webhookEventId`, como se vê em
`internal/adapters/rabbitmq/publisher.go:110-117`. O broker, as filas de retry e
a DLQ não contêm uma cópia do payload. Eles ainda carregam identificadores
operacionais que podem ser relacionados à inbox e continuam sujeitos a controle
de acesso e disposição; o PostgreSQL é apenas o único sistema com o evento
completo.

## Decisão

### 1. O projeto persiste dados pessoais, e isso é declarado

Nenhum documento do repositório pode afirmar, direta ou indiretamente, que a
aplicação não armazena dados pessoais. O critério correto é o inverso: a
instalação armazena dados pessoais recebidos do provedor de pagamento. A
organização que determina a finalidade e os meios normalmente atua como
controladora; cada implantação precisa definir seus papéis jurídicos, pois
operar a infraestrutura não torna alguém automaticamente controlador.

O que o projeto continua **não** armazenando é o dado completo de cartão. Isso
é uma propriedade do desenho — o número nunca chega à aplicação, porque o
Checkout hospedado da Stripe o coleta — e não decorre de nenhum tratamento feito
aqui.

### 2. Inventário: onde cada cópia existe

| # | Local | Conteúdo | Contém dado pessoal | Finalidade |
| --- | --- | --- | --- | --- |
| 1 | Requisição HTTP de webhook (memória) | Evento completo | Sim | Verificar assinatura sobre os bytes originais |
| 2 | `webhook_events.raw_payload` | Corpo exato cuja assinatura foi verificada | Sim | Auditoria e diagnóstico do corpo recebido; não permite reverificação isoladamente |
| 3 | `webhook_events.payload` | Evento parseado | Sim | Processamento, reprocessamento e consulta |
| 4 | `webhook_events.last_error` | Texto de erro truncado | Por acidente, se não sanitizado | Diagnóstico operacional |
| 5 | `outbox_events.last_error` | Texto de erro truncado | Por acidente, se não sanitizado | Diagnóstico da publicação |
| 6 | `outbox_events` (demais colunas) | Referência e correlação do evento | Potencialmente, por correlação | Publicação transacional |
| 7 | Mensagem no RabbitMQ | Referência e correlação do evento | Potencialmente, por correlação | Transporte |
| 8 | Filas de retry e DLQ | A mesma referência da mensagem | Potencialmente, por correlação | Retentativa e quarentena |
| 9 | Respostas de `GET /v1/webhook-events*` | IDs, metadados + `lastError` | Potencialmente; diretamente via `lastError` não sanitizado | Inspeção autenticada |
| 10 | Logs da aplicação | Método, path, correlação e erros | Potencialmente; não deve conter payload, credencial ou entrada livre | Operação |
| 11 | Traces OTLP | Atributos de `otelhttp` + serviço | Potencialmente; não deve conter payload, credencial ou entrada livre | Observabilidade |
| 12 | Backups do PostgreSQL | Cópia integral do banco | Sim | Recuperação |
| 13 | `orders` | ID, produto, quantidade, valores e chave de idempotência livre | Potencialmente | Estado comercial e idempotência |
| 14 | `payments` | IDs, provedor, estado e valores | Potencialmente, por correlação | Estado financeiro |
| 15 | `payment_attempts` | Chave de idempotência, IDs da Stripe, URL de Checkout e mensagem de falha | Potencialmente; URL e entrada livre são confidenciais | Execução e diagnóstico do checkout |

Linhas 6, 7 e 8 são consequência direta do ADR 0011 e devem permanecer sem o
payload do provedor. Isso reduz a exposição, mas não retira do escopo de
proteção identificadores que possam ser relacionados à inbox. Adicionar payload
à mensagem publicada reabriria a decisão inteira.

### 3. Finalidade, acesso e retenção por cópia

**`raw_payload` (cópia 2).** Finalidade: preservar o corpo exato recebido para
auditoria, diagnóstico e comparação com a cópia parseada, depois de a assinatura
ter sido verificada durante a requisição. Acesso: somente conexão direta ao
banco; nenhuma rota da API o expõe, e nenhuma deve passar a expor. O header de
assinatura não é persistido, e a coluna não permite reverificação posterior nem
prova independente de origem. Retenção recomendada: 90 dias após
`processed_at`; a Stripe permanece como fonte externa para recuperação.

**`payload` (cópia 3).** Finalidade: processar e reprocessar o evento. Acesso:
o consumer, e conexão direta ao banco para diagnóstico. Retenção recomendada:
90 dias após `processed_at`, igual a `raw_payload` — as duas cobrem a mesma
janela de utilidade e não faz sentido expurgar uma sem a outra.

**`last_error` em ambas as tabelas (cópias 4 e 5).** Finalidade: diagnóstico.
Acesso: banco e API operacional autenticada. Retenção: acompanha a linha. A
regra que vale aqui não é de retenção e sim de conteúdo, na seção 5.

**Eventos `failed` (cópias 2 e 3, subconjunto).** Não são expurgados pelo prazo.
Uma linha `failed` é trabalho pendente: ela ainda pode ser reprocessada por
`POST /v1/webhook-events/{id}/reprocess`, e apagá-la destrói tanto o efeito
financeiro não aplicado quanto o registro de que ele ficou faltando. O expurgo
de um evento `failed` exige decisão operacional explícita, registrada, e vem
depois de resolver ou aceitar conscientemente a falha. O mesmo vale para
`pending` e `processing`.

**Backups (cópia 12).** Um backup contém tudo que a política acima expurga, e
por isso o expurgo no banco primário não é suficiente sozinho. Retenção
recomendada: 30 dias, com descarte automático pelo provedor de infraestrutura.
Backups herdam a classificação do dado mais sensível que contêm — neste caso,
dados pessoais — e precisam de cifragem em repouso e do mesmo controle de acesso
do banco. Em uma implantação Railway isso é configuração do provedor, não do
código deste repositório.

**DLQ (cópias 7 e 8).** Não contém o payload nem dados diretos do cliente, mas
carrega referências potencialmente relacionáveis à inbox. Sua retenção é
principalmente operacional: uma mensagem parada na DLQ é um incidente aberto, e
o critério é resolvê-la, não expirá-la. Purgar a DLQ apaga a única lista de
eventos que ficaram sem efeito.

**Logs e traces (cópias 10 e 11).** Nenhum dos dois pode conter payload, chave,
credencial, identificador fornecido livremente ou dado direto do cliente. Paths
e correlações ainda podem ser pseudônimos relacionáveis. A regra de conteúdo é
verificada em E8b, e a retenção é a do backend escolhido pela implantação.

### 4. Expurgo: procedimento operacional, não automação, na 0.1.0

A `0.1.0` **não** terá job de expurgo automático. A decisão é entregar um
procedimento operacional documentado, com o SQL de expurgo, os prazos acima e a
regra sobre eventos não terminais.

O motivo é que um expurgo automático é destrutivo, irreversível e roda sem
supervisão. Nesta versão faltam três coisas para que ele seja seguro: métrica de
quantas linhas ele apagaria, teste de sistema que prove que ele não toca linhas
não terminais, e experiência operacional sobre o volume real de uma instalação.
Um job errado apaga eventos financeiros pendentes de forma silenciosa; o
procedimento manual erra do lado de reter demais, que é o erro reversível.

O que **não** é aceitável, e é o que existe hoje, é retenção indefinida
implícita. A ausência de expurgo passa a ser uma decisão escrita, com prazo
recomendado e procedimento publicado, e não um comportamento silencioso.

A automação é reavaliada quando as métricas de E5 estiverem estáveis. O trabalho
está registrado como `E8A-7` no backlog oficial de `docs/security.md`.

### 5. `last_error` é superfície pública e precisa ser sanitizado

O campo é tratado como se fosse exposto a um integrador, porque é. Regras:

- sanitizar **antes** de truncar, dentro do próprio `errorText`, que já é o
  ponto único por onde os dois caminhos passam; truncar em 500 bytes um texto
  que começa com uma DSN não remove nada de sensível;
- remover credenciais em URLs de PostgreSQL e RabbitMQ, valores de
  `X-API-Key`, chaves da Stripe e qualquer trecho de payload do provedor;
- não incluir resposta integral do provedor;
- preservar utilidade: a mensagem continua identificando a classe do erro e
  carrega correlação suficiente para achar o resto nos logs, que são a
  superfície privada.

O par correspondente é a invariante já implementada em
`db/queries/operations.sql`: nenhuma query da API operacional seleciona
`raw_payload` ou `payload`. Isso vira teste em E3/E8b, incluindo teste negativo
sobre valores sentinela.

### 6. Responsabilidade em implantação self-hosted

Este repositório entrega o desenho e o procedimento; não entrega conformidade.
Cada implantação define quem determina finalidade e meios, quem processa os
dados em seu nome e as responsabilidades contratuais correspondentes. Também
são responsabilidades da implantação: cifragem em repouso, retenção efetiva de
backups, controle de acesso ao banco e ao broker, execução do expurgo, base
legal para o tratamento, resposta a titulares e resposta a incidentes.

## Modelo de ameaça

Os atacantes considerados e o que o projeto opõe a cada um.

**Integrador hostil** — possui uma `X-API-Key` válida e usa a API operacional
além da intenção. Pode enumerar `GET /v1/webhook-events` e ler `lastError` de
qualquer evento. O projeto opõe: exclusão de payloads nas queries operacionais,
sanitização de `last_error` (E8b) e rate limiting (E7). O projeto **não** opõe
segregação por tenant: qualquer chave válida enxerga todos os eventos da
instalação.

**Terceiro na internet** — sem credencial. Alcança as rotas públicas: health,
o webhook e o Checkout. Pode enviar corpos inválidos ou grandes e tentar forjar
eventos. O projeto opõe: verificação de `Stripe-Signature` sobre os bytes
originais antes de desserializar ou persistir, limite de corpo, allowlist de
rotas públicas do [ADR 0010](0010-route-access-model.md) e rate limiting (E7).
Um corpo acima do limite ainda permite provocar erro deliberadamente; é
limitação conhecida do ADR 0011.

**Pessoa com acesso operacional** — acessa banco, broker, logs e traces
legitimamente. Pode correlacionar identificadores e acessar todos os dados
pessoais persistidos no banco e nos backups. O projeto opõe apenas minimização:
payloads fora de logs, traces e mensagens, com o evento completo apenas no
PostgreSQL. Não há mascaramento por coluna, cifragem em nível de aplicação nem
trilha de auditoria de leitura no banco. É a exposição mais ampla que resta, e
é assumida.

**Provedor comprometido, ou alguém que capture uma entrega** — o
`STRIPE_WEBHOOK_SECRET` é a única prova de origem. Se ele vazar, eventos forjados
são aceitos e produzem efeito financeiro. O projeto opõe: tratamento como
secret, procedimento de rotação (E8b) e a inbox, que preserva os bytes recebidos
e torna o evento forjado auditável depois. Não há segunda prova de origem.

### O que o projeto explicitamente não protege

- **Multi-tenant.** Uma chave de integração é uma credencial da instalação
  inteira, não de um cliente. Não use uma instalação para servir clientes que
  não podem ver os dados uns dos outros.
- **PCI DSS.** Dados completos de cartão nunca chegam à aplicação porque o
  Checkout é hospedado, mas usar este projeto não certifica nada, e o escopo de
  PCI é da implantação.
- **LGPD e GDPR.** O projeto documenta onde os dados estão e por quanto tempo.
  Base legal, atendimento a titulares, registro de operações e transferência
  internacional são da implantação.
- **Fraude e chargeback.** Não há scoring, regra de risco ou fluxo de disputa.
- **Abuso interno do integrador.** Uma chave válida usada de má-fé não é
  distinguida de uma usada corretamente, além do rate limiting.
- **Disponibilidade sob ataque volumétrico.** Rate limiting protege contra abuso
  ordinário; não é defesa contra DDoS.

## Alternativas consideradas

### Não persistir `raw_payload`, guardando apenas `payload`

Rejeitada. Elimina uma cópia, mas perde o corpo exatamente como recebido:
`jsonb` reordena chaves e normaliza formatação. Preservar esses bytes melhora a
auditoria e permite comparar o recebido com a representação processada, embora
não permita reverificar a assinatura sem o header que não é persistido.

### Cifrar as colunas de payload em nível de aplicação

Rejeitada nesta fase. Move o problema para gestão de chave, que este projeto não
tem, e quebra consulta e diagnóstico por SQL, que é hoje o único acesso ao
payload. Cifragem em repouso no provedor cobre o cenário de disco ou backup
vazado, que é o mais provável, sem esse custo. Uma cifragem que protegesse
contra o acesso operacional interno exigiria custódia de chave fora da
aplicação, e isso é decisão de implantação.

### Remover `lastError` da resposta da API operacional

Rejeitada. É o campo que torna a inspeção útil: sem ele, o integrador vê que um
evento falhou e não vê por quê, e a única saída passa a ser acesso ao banco —
o que amplia a exposição em vez de reduzi-la. A resposta correta é sanitizar o
conteúdo, não esconder o campo.

### Expurgo automático já na 0.1.0

Rejeitada pelos motivos da seção 4: destrutivo, irreversível, sem métrica e sem
teste de sistema que prove que não toca linhas não terminais.

### Retenção indefinida assumida

Rejeitada. É o comportamento atual e é o que esta ADR corrige. Guardar dado
pessoal para sempre por omissão é a pior posição possível: nenhuma decisão foi
tomada, e ninguém sabe que havia uma a tomar.

## Consequências

### Positivas

- Toda cópia de payload e todo campo de erro persistido tem finalidade, acesso
  e retenção escritos.
- A retenção deixa de ser silenciosa: existe prazo recomendado e procedimento.
- `last_error` passa a ser tratado como superfície pública, com regra de
  conteúdo verificável.
- A exclusão de payloads nas queries operacionais passa de intenção comentada a
  invariante testada.
- O escopo do modelo de ameaça delimita o que a instalação precisa cobrir por
  conta própria.
- O broker permanece sem uma cópia do payload; suas referências continuam
  protegidas como dados operacionais potencialmente relacionáveis.

### Limitações

- O expurgo depende de execução manual; uma instalação que não o executar
  mantém dados pessoais indefinidamente, agora conscientemente.
- Não há segregação multi-tenant, e nenhuma decisão desta fase a introduz.
- Acesso operacional direto ao banco continua vendo todos os dados pessoais.
- A retenção efetiva de backups é do provedor de infraestrutura e não é
  verificável pela CI deste repositório.
- Sanitizar `last_error` reduz o detalhe disponível a quem depende só da API;
  o detalhe completo passa a exigir os logs.
- Os prazos de 90 e 30 dias são recomendações de projeto, não obrigações legais;
  cada implantação precisa avaliar as suas.

## Estado de implementação

E8b aplicou no código o que esta ADR decide:

- sanitização dentro de `errorText`, em
  `internal/adapters/postgres/repositories/outbox.go`, aplicada antes do
  truncamento de 500 bytes, via `internal/platform/errsanitize.Sanitize`. A
  função continua sendo o ponto único de gravação usado tanto pelo caminho da outbox
  (`Retry`/`Failed`) quanto pelo `RecordFailure` da inbox — nenhuma nova
  fronteira foi criada. O sanitizador preserva UTF-8 válido ao truncar e usa
  o marcador estável `[REDACTED]`. O mesmo tratamento foi estendido, além do
  que esta ADR exigia originalmente, à leitura da API operacional, aos logs
  estruturados da API, do relay, do consumer e das migrations
  (`internal/platform/logging.SanitizedError`), e à saída fatal em `stderr` dos
  binários, porque um erro de driver ou de broker pode carregar a mesma
  credencial em qualquer uma dessas superfícies;
- teste negativo com valores sentinela sobre logs (API, relay e consumer),
  atributos e eventos de trace OpenTelemetry, `last_error` (outbox e inbox) e
  respostas HTTP públicas — ver
  `internal/transport/http/errsanitize_negative_test.go`,
  `internal/runtime/worker/observers_test.go` e os testes de sanitização em
  `internal/adapters/postgres/repositories/{events,outbox}_test.go`;
- teste que impede qualquer query operacional de selecionar `raw_payload` ou
  `payload`, verificando tanto os tipos de linha gerados pelo sqlc quanto o
  texto-fonte de `db/queries/operations.sql` — ver
  `internal/adapters/postgres/repositories/operations_sql_test.go`;
- procedimento de rotação de `INTEGRATION_API_KEYS`, `STRIPE_SECRET_KEY` e
  `STRIPE_WEBHOOK_SECRET`, e registro de minimização/retenção de URLs de
  Checkout e chaves de idempotência, publicados em `docs/security.md`.

O procedimento de expurgo de payloads já existia em `docs/security.md` antes
de E8b (seção "Política de retenção") e não foi alterado por esta etapa. Ele
ainda não é referenciado por um guia operacional dedicado, porque esse guia é
entrega de E9 e não existe nesta versão; a purga automatizada/temporizada de
`payment_attempts.checkout_url` e das chaves de idempotência também permanece
para E9 ou pós-`0.1.0`, como registrado no backlog oficial de
`docs/security.md` (`E8A-6`, `E8A-7`, `E8A-9`).

Os itens derivados usam identificadores estáveis e estão registrados no backlog
oficial de `docs/security.md`; não há alegação de que já existam issues externas.

## Referências

- [ADR 0010: modelo de acesso às rotas](0010-route-access-model.md)
- [ADR 0011: contrato de recepção de webhooks e conteúdo da outbox](0011-webhook-reception-and-outbox.md)
- [ADR 0013: transação, transições e política de falha do consumer](0013-consumer-transactions-transitions-and-retry.md)
- [Stripe — objeto Checkout Session e `customer_details`](https://docs.stripe.com/api/checkout/sessions/object)
- [Stripe — verificação de assinatura de webhooks](https://docs.stripe.com/webhooks#verify-official-libraries)
- [Stripe — retenção e reenvio de eventos](https://docs.stripe.com/webhooks#events-overview)
