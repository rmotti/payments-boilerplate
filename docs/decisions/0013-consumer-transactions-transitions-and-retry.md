# ADR 0013: transação, transições e política de falha do consumer

- Status: aceito
- Data: 2026-09-07
- Implementação: em andamento na terceira entrega da Fase 3

## Contexto

O [ADR 0011](0011-webhook-reception-and-outbox.md) fez a API gravar o evento
recebido e sua mensagem de outbox na mesma transação. O
[ADR 0012](0012-outbox-relay-and-topology.md) fez o relay publicá-la no
RabbitMQ com publisher confirms. Nada consome essas mensagens: o pagamento
termina como `paid` na Stripe e o pedido continua `pending` localmente.

Esta entrega fecha três itens do roadmap de uma vez — consumo com ack manual e
concorrência limitada, retry com backoff e dead-letter queue, e impedimento de
regressões inválidas de estado. Os três são uma entrega só porque nenhum deles
é correto sozinho: um consumer sem política de falha perde eventos ou entra em
loop, e transições sem proteção contra concorrência e reordenação corrompem o
estado financeiro no primeiro evento fora de ordem.

Quatro questões precisam ser fechadas antes do código porque todas viram
contrato. A fronteira entre GORM e `sqlc` determina o desenho da transação. A
matriz de transições determina o que `GET /v1/orders/{orderId}` passa a
responder. A topologia de retry vira imutável assim que a primeira fila for
declarada. E a janela entre o commit no PostgreSQL e o ack no broker determina
quantas vezes um efeito pode se repetir.

Duas restrições vêm de decisões já tomadas e não são negociáveis aqui. A
`payments.webhooks` já existe com `x-dead-letter-exchange` apontando para
`payments.webhooks.dlx`, e os argumentos de uma fila são imutáveis no
RabbitMQ — o retry precisa ser construído com objetos novos. E a Stripe não
garante ordem de entrega, então a matriz de transições não pode depender da
sequência de chegada.

## Decisão

### O caminho de escrita do consumer é exclusivamente `sqlc`

O consumer lê a inbox, resolve o agregado, aplica as transições e marca o
evento como processado em uma única transação aberta sobre `database/sql`, com
as queries geradas pelo `sqlc`. GORM não participa dessa transação.

Isso não é uma exceção ao [ADR 0007](0007-gorm-and-sqlc.md); é a aplicação
direta do seu critério. Ele reserva `sqlc` para "quando a forma da query fizer
parte da garantia do sistema, incluindo inbox, outbox, transições condicionais,
`FOR UPDATE`, `SKIP LOCKED`". A transação do consumer é feita inteiramente
dessas coisas. As convenções, a arquitetura e o README dos repositories já
registram a mesma expectativa desde a Fase 2.

A consequência precisa ser dita explicitamente, porque à primeira vista parece
descuido: a fronteira entre os dois estilos deixa de ser por tabela e passa a
ser por query. As tabelas `payments`, `payment_attempts` e `orders` passam a ter
dois caminhos de escrita — GORM no checkout da API, `sqlc` no consumer do
worker. É consistente com o ADR 0007, cujo critério sempre foi a garantia da
query e nunca a propriedade da tabela, mas exige que quem alterar o schema
lembre de ambos.

### O lock cobre o agregado inteiro, não apenas o evento

Bloquear apenas a linha da inbox serializa duas entregas do mesmo evento e
nada mais. Não é suficiente: eventos **diferentes** podem tocar o mesmo
pagamento ao mesmo tempo, e é justamente isso que a concorrência do consumer
torna provável. Um `checkout.session.completed` e um
`checkout.session.expired` da mesma sessão, processados em paralelo, leriam o
mesmo estado inicial e aplicariam transições incompatíveis.

A transação bloqueia, em ordem fixa:

```text
webhook_event   FOR UPDATE
      ↓
payment_attempt FOR UPDATE
      ↓
payment         FOR UPDATE
      ↓
order           FOR UPDATE
```

O lock do `payment` e do `order` é o que serializa eventos distintos do mesmo
pagamento. O lock do `webhook_event` continua existindo, mas cobre outra coisa:
a entrega repetida do mesmo evento, que é o caso de uma redelivery ou de uma
republicação do relay.

A ordem é fixa e sempre a mesma para evitar deadlock. Duas transações que
adquirissem os mesmos locks em ordens diferentes travariam uma na outra, e o
PostgreSQL abortaria uma delas — recuperável, já que um deadlock é falha
transitória e a mensagem volta, mas é trabalho desperdiçado e ruído
operacional que a ordem fixa elimina de graça.

Os locks são bloqueantes, não `SKIP LOCKED`. `SKIP LOCKED` é certo para o
relay, onde pular uma mensagem que outra instância já pegou é exatamente o
comportamento desejado. Aqui seria errado: pular significaria dar ack em um
evento cujo efeito ninguém aplicou, e se a transação concorrente falhasse o
efeito se perderia. Bloquear custa alguns milissegundos e é sempre correto.

### Zero linhas atualizadas não significa no-op

O rascunho desta entrega tratava `:execrows` retornando zero como "transição
não aplicável". É insuficiente e perigoso: quatro situações muito diferentes
produzem zero linhas.

| Situação | Significado | Desfecho |
| --- | --- | --- |
| Estado já é o desejado | Efeito já aplicado | No-op, `processed` |
| Transição regressiva | Evento fora de ordem | No-op, `processed` |
| Entidade não existe ou metadata errada | Inconsistência | Terminal, DLQ |
| Estado mudou depois do lock | Invariante violada | Terminal, DLQ |

Confundir as duas primeiras com as duas últimas esconde uma inconsistência
real dentro do ruído de eventos fora de ordem, que são normais e frequentes.
Confundir as duas últimas com as duas primeiras manda para a DLQ um evento que
simplesmente chegou atrasado.

A decisão que separa os casos: **o serviço lê o estado já bloqueado, consulta a
matriz de transições e decide o desfecho antes de escrever**. Só depois que a
matriz disser "aplicar" é que o `UPDATE` condicional roda — e nesse ponto ele
precisa afetar exatamente uma linha. Zero linhas **depois** da decisão não é
no-op: é uma invariante violada sob um lock que deveria tê-la impedido, e vai
para a DLQ.

O `WHERE` condicional do `UPDATE` permanece mesmo assim. Ele é a segunda linha
de defesa: se um dia a matriz e o SQL divergirem, a constraint no `WHERE` é o
que impede a regressão de ser gravada, e a contagem de linhas é o que denuncia
a divergência.

### A matriz de transições

Um evento move três entidades de uma vez. O efeito normal, quando o evento
chega em um estado que o aceita:

| Evento | `payment_attempt` | `payment` | `order` |
| --- | --- | --- | --- |
| `checkout.completed`, `payment_status` pago | `succeeded` | `succeeded` | `paid` |
| `checkout.completed`, `payment_status` não pago | permanece `pending` | `processing` | permanece `pending` |
| `checkout.payment_succeeded` | `succeeded` | `succeeded` | `paid` |
| `checkout.payment_failed` | `failed` | `failed` | permanece `pending` |
| `checkout.expired` | `expired` | permanece `pending` | permanece `pending` |

Quatro regras governam tudo que não é o caso normal:

- **Evento negativo depois de um estado final positivo é no-op.** Um `expired`
  que chega depois de `succeeded` não regride nada. A sessão de fato expirou;
  o pagamento de fato aconteceu antes.
- **Evento de uma tentativa antiga já encerrada não altera o pagamento.** Uma
  tentativa `expired` ou `cancelled` pode já ter liberado uma nova sessão no
  mesmo pagamento. `completed` sem liquidação, `failed` e `expired` tardios são
  no-op para não interferir na tentativa nova; sucesso contraditório é terminal.
- **Repetição do mesmo resultado é no-op.** É o caso de toda redelivery, e é
  o que torna o consumer idempotente.
- **Resultado positivo chegando sobre um estado final contraditório é
  terminal.** Um `payment_succeeded` sobre um pagamento `failed` não é um
  evento atrasado: é uma inconsistência entre o que a Stripe acredita e o que
  está gravado aqui, e ignorá-la em silêncio esconde exatamente o tipo de
  divergência que o projeto se compromete a manter em zero. O mesmo vale para
  uma tentativa `failed`, `expired` ou `cancelled`: uma tentativa antiga não
  pode concluir o pagamento enquanto outra sessão pode estar ativa. Vai para a
  DLQ.
- **Reembolso nunca é regredido por um evento de checkout.** `refunded` e
  `partially_refunded` já pertencem ao vocabulário persistido. Um evento tardio
  é no-op, e as condições SQL também impedem voltar esses estados para
  `processing`, `failed` ou `succeeded`.
- **Divergência de valor, moeda, provider ou relação entre identificadores é
  terminal.** Nunca é reprocessável, e aplicar o efeito seria pior que parar.

`checkout.session.completed` com `payment_status` não pago produzir
`processing` não é uma sutileza: é o caso do Pix e de todo meio de confirmação
tardia, em que a sessão completa antes do dinheiro chegar e só o evento
assíncrono posterior conclui o pagamento. Tratar `completed` como sinônimo de
pago funcionaria com cartão e daria pedido pago sem pagamento no primeiro Pix.

A tentativa permanece aberta nesse caso porque ela ainda é a tentativa válida:
é sobre ela que o `async_payment_succeeded` ou `async_payment_failed` vai
chegar.

### Sessão expirada não cancela o pagamento

`checkout.session.expired` move a tentativa para `expired` e deixa `payment` e
`order` em `pending`.

O índice parcial `payment_attempts_payment_id_active_key` exclui `expired`, de
modo que expirar a tentativa já libera uma nova dentro do mesmo pagamento. E o
checkout atual recupera o pagamento ativo do pedido em vez de criar outro, em
`createOrGetActivePayment`. Marcar o `payment` como `cancelled` sairia do
índice parcial `payments_order_id_active_key` e faria o próximo checkout abrir
um segundo pagamento para o mesmo pedido — sem necessidade, e criando
histórico financeiro que não corresponde a nada que aconteceu.

O pagamento é a operação financeira do pedido; a tentativa é a interação com o
provedor. Uma sessão que expirou encerrou a interação, não a operação.

### Retry: faixas fixas em filas próprias, contagem na inbox

A fila principal não pode ser reconfigurada, então o retry é feito de objetos
novos:

```text
payments.events (topic)
  └── payment.webhook.#  →  payments.webhooks
                              ├── falha transitória → republicação explícita
                              └── falha terminal    → republicação explícita

payments.webhooks.retry.5s   (fanout) → fila quorum, TTL 5s   ─┐
payments.webhooks.retry.30s  (fanout) → fila quorum, TTL 30s  ─┤
payments.webhooks.retry.2m   (fanout) → fila quorum, TTL 2m   ─┼→ payments.events
payments.webhooks.retry.10m  (fanout) → fila quorum, TTL 10m  ─┘

payments.webhooks.dlx (topic) → payments.webhooks.dlq
```

Três escolhas dentro dessa topologia decidem se ela funciona:

**Uma fila por faixa, não uma fila com TTL por mensagem.** O TTL de fila do
RabbitMQ expira mensagens em ordem de chegada: a fila só olha para a cabeça.
Uma mensagem com espera longa na frente segura todas as de espera curta atrás
dela, e o backoff exponencial deixa de existir. Uma fila por faixa custa
quatro objetos e elimina o problema por construção.

**Exchanges fanout, não topic.** A mensagem precisa voltar para
`payments.events` com sua routing key original, ou não casa o binding
`payment.webhook.#`. O dead-lettering preserva a routing key original quando a
fila não define `x-dead-letter-routing-key`, e publicar em uma fanout não
consome nem exige routing key. Uma exchange topic obrigaria a inventar uma
chave de roteamento para a faixa e a perder o tipo de domínio no caminho.

**A contagem de tentativas vem de `webhook_events.attempts`, não do header
`x-death`.** A coluna já existe, é durável, sobrevive à perda da mensagem, é a
mesma que o operador consulta ao investigar e é gravada dentro da transação
que já está aberta. O `x-death` continua chegando e serve para corroborar,
nunca como fonte.

As filas de retry são quorum, com `x-dead-letter-strategy: at-least-once` e
`x-overflow: reject-publish`. Isso importa porque a volta da faixa para
`payments.events` é um dead-lettering, e o dead-lettering de fila clássica
republica **sem** publisher confirms: a mensagem sai da fila de retry assim que
é publicada e se perde se o destino não puder aceitá-la. A fila quorum com essa
estratégia republica com confirms internos e só remove a mensagem depois do
confirm. As filas de retry são objetos novos, então podem nascer assim sem
tocar em nada declarado.

`reject-publish` não é opcional: o `at-least-once` não funciona com o
`drop-head` padrão, porque descartar a cabeça é exatamente a perda que a
estratégia existe para impedir. A consequência é que uma fila de retry que
atingisse `x-max-length` passaria a rejeitar publicações em vez de descartar
mensagens antigas — e a rejeição chega ao consumer como falha de republicação,
que já tem caminho definido: sem ack, canal fechado, redelivery. Perder um
evento financeiro silenciosamente seria pior que reprocessá-lo.

`CONSUMER_RETRY_DELAYS` define as faixas, e a última se repete até
`CONSUMER_MAX_ATTEMPTS`. Diferente do relay, o consumer **tem** limite de
tentativas: uma falha transitória do relay é sempre infraestrutura, que termina
sozinha, enquanto uma falha transitória do consumer pode ser um bug de
aplicação que nunca termina. A DLQ é onde ela fica visível.

### A ordem das operações, e o que cada janela duplica

O ack é a última operação, sempre, e cada caminho é explícito:

```text
sucesso:
  COMMIT
  → ack

falha transitória:
  ROLLBACK
  → transação curta: attempts + 1, last_error; status failed se esgotou budget
  → publicar na faixa de retry, persistente, mandatory, com confirm
  → ack

falha terminal:
  ROLLBACK
  → transação curta: status failed, last_error
  → publicar na DLX, persistente, mandatory, com confirm
  → ack
```

**A republicação é explícita e confirmada, inclusive para a DLQ.** O caminho
óbvio para a dead-letter seria `nack(requeue=false)` e deixar o dead-lettering
da fila principal levar a mensagem. Ele é rejeitado porque a `payments.webhooks`
é uma fila clássica, e o dead-lettering de fila clássica republica sem publisher
confirms: a mensagem sai da fila principal assim que é publicada na DLX e se
perde se a DLQ não puder aceitá-la. Uma mensagem perdida a caminho da DLQ é a
pior perda possível, porque a DLQ é precisamente o lugar para onde vão as
coisas que precisam de atenção humana.

Converter a fila principal em quorum resolveria isso na origem, mas exigiria
apagá-la e recriá-la — a operação que o ADR 0012 recusou sobre uma fila que
carrega mensagens financeiras. A republicação explícita entrega a mesma
garantia sem tocar na fila. O `x-dead-letter-exchange` da fila principal
permanece declarado como rede de segurança para o caso de um `nack` acontecer
por outro motivo, mas não é o caminho da aplicação. Até uma mensagem cujo corpo
não contém `webhookEventId` é republicada com os bytes originais e confirmada
antes do ack; a ausência da referência impede registrar a inbox, não impede
preservar a mensagem para diagnóstico.

**Se aplicar, registrar a falha, republicar ou dar ack falhar, a mensagem
original não recebe ack.** O worker reporta o erro à sessão, o canal inteiro é
fechado e o supervisor reconecta com backoff. Fechar é necessário: deixar o
canal aberto manteria a entrega indefinidamente em voo e, depois de ocupar todo
o prefetch, pararia o consumer. A entrega volta pelo próprio mecanismo de
redelivery do broker, sem `nack(requeue=true)` — que as convenções proíbem
justamente porque devolveria a mensagem à cabeça da fila para falhar de novo em
microssegundos.

O incremento de `attempts` e a decisão `pending`/`failed` também são um único
`UPDATE`. Se o serviço decidisse que o budget acabou somente depois de gravar
uma falha transitória, seria possível confirmar a cópia na DLQ e deixar a inbox
como `pending`, sem mensagem correspondente na fila principal.

Duas janelas de duplicação permanecem, e ambas são inerentes à entrega pelo
menos uma vez:

- Entre o `COMMIT` e o ack, uma queda reentrega o evento. A segunda passagem
  encontra o `webhook_event` já `processed` e sai como no-op. É a razão de o
  status da inbox ser lido sob lock antes de qualquer outra coisa.
- Entre a publicação do retry e o ack, uma queda reentrega o original e a
  cópia de retry existe. As duas convergem: a que chegar primeiro processa, a
  segunda vira no-op.

### Relay e consumer usam conexões AMQP separadas

O worker abre três conexões: uma para o publisher do relay, uma para o
consumer e uma para as republicações de retry e DLQ.

A razão é concreta e está na implementação atual. O `Connection` impõe
deadlines no socket inteiro durante cada operação, sob um mutex, porque um
socket tem um único par de deadlines e o driver AMQP não aplica deadline por
chamada. Isso é correto e necessário para o relay, mas significa que uma
tentativa de publicação com orçamento de cinco segundos aplicaria esse
deadline ao socket compartilhado — e um consumer, que fica legitimamente
bloqueado esperando a próxima entrega por tempo indeterminado, veria seu
canal morrer.

Separar o publisher de retry do publisher do relay tem outra razão: uma
republicação de retry acontece dentro do tratamento de uma mensagem, com o
worker de consumo parado esperando por ela. Compartilhar o mutex de operação
com o ciclo do relay faria a latência de um depender da saúde do outro.

## Alternativas consideradas

### Bridge compartilhando o mesmo `sql.Tx` entre GORM e `sqlc`

Rejeitada por ora, e é a alternativa mais próxima de ser aceita. É viável e
barata: o GORM é aberto sobre o mesmo `*sql.DB`, então dentro de
`Transaction(func(tx *gorm.DB))` o `tx.Statement.ConnPool` é o próprio
`*sql.Tx`, que pode ser passado a `dbgen.New`. O ADR 0007 já prevê essa
possibilidade, exigindo que seja explícita e testada.

Não é construída porque nada precisa dela. A transação do consumer não tem uma
única operação de CRUD comum, e a bridge acrescentaria uma asserção de tipo
sobre um detalhe interno do GORM em um caminho financeiro crítico. Ela é a
saída caso um caso de uso futuro precise de CRUD do GORM dentro de uma
transação crítica.

### Duas transações, uma por estilo de acesso

Rejeitada. Deixaria uma janela em que o efeito de negócio foi gravado e o
evento não foi marcado como processado, ou o inverso. A primeira produz efeito
duplicado na redelivery; a segunda perde o efeito.

### `nack(requeue=false)` como caminho normal para a DLQ

Rejeitada pelo motivo descrito acima: a republicação interna do dead-lettering
não é confirmada, e uma mensagem pode desaparecer no trajeto para a DLQ.

### `nack(requeue=true)` para falhas transitórias

Rejeitada, e proibida pelas convenções. Devolve a mensagem imediatamente, sem
backoff, produzindo um loop apertado contra a mesma falha.

### Fila de retry única com TTL por mensagem

Rejeitada. Topologia menor, mas o TTL de fila expira em ordem de chegada e uma
mensagem de espera longa na cabeça bloqueia as curtas atrás dela. Só seria
correta com atraso fixo, o que elimina o backoff.

### Plugin de delayed message exchange

Rejeitada. Resolve o problema com um objeto só, mas adiciona uma dependência
de plugin que nem todo broker gerenciado oferece, em um projeto cuja proposta é
subir com o RabbitMQ padrão do compose.

### Backoff no PostgreSQL, com re-drive da inbox

Adiada. A inbox já tem `attempts`, `last_error` e um índice parcial sobre
`status IN ('pending', 'failed')` que nada usa ainda, e um poller daria backoff
exato e inspeção de graça. Mas é essencialmente o item "permitir inspeção e
reprocessamento seguro", que é outra entrega, e deixaria esta sem a
dead-letter queue que o roadmap pede nominalmente. As duas convivem depois: o
re-drive da inbox é o caminho de recuperação de uma mensagem que chegou à DLQ.

### Bloquear apenas o `webhook_event`

Rejeitada, e era o desenho do rascunho desta entrega. Serializa a entrega
repetida do mesmo evento, que é o caso fácil, e deixa passar o caso difícil:
dois eventos diferentes do mesmo pagamento processados em paralelo, lendo o
mesmo estado inicial e aplicando transições incompatíveis.

### Marcar o `payment` como `cancelled` na expiração da sessão

Rejeitada. Tiraria o pagamento do índice parcial de pagamento ativo e faria o
próximo checkout do mesmo pedido abrir um segundo pagamento, quando o primeiro
nunca chegou a falhar — apenas a sessão do provedor expirou.

### Uma conexão AMQP compartilhada entre relay e consumer

Rejeitada. Os deadlines de socket que a publicação instala matariam um canal de
consumo legitimamente ocioso.

## Consequências

### Positivas

- Efeito de negócio e marcação do evento são atômicos: nunca existe um sem o
  outro.
- Eventos fora de ordem, que a Stripe explicitamente não ordena, não regridem
  estado nem produzem falha.
- Eventos diferentes do mesmo pagamento não podem se atropelar, porque o lock
  alcança o agregado e não apenas a linha da inbox.
- Uma inconsistência real fica distinguível de um evento atrasado, tanto no
  log quanto no destino da mensagem.
- Nenhuma mensagem chega à DLQ sem confirmação do broker.
- Nenhum caminho de falha devolve a mensagem sem backoff.
- A fila principal e sua dead-letter permanecem exatamente como declaradas,
  sem recriação.
- O Pix é suportado pela matriz desde o primeiro dia, mesmo antes de ser
  habilitado.

### Limitações

- O lock do agregado serializa o processamento por pagamento. A concorrência
  configurada só rende quando os eventos em voo tocam pagamentos diferentes,
  o que é o caso normal, mas uma rajada sobre um único pedido processa em
  série.
- Quatro filas de retry são quatro objetos a mais para operar e observar.
- O limite de tentativas do consumer pode mandar para a DLQ um evento cuja
  falha era transitória mas durou mais que a soma das faixas. É deliberado:
  diferente do relay, uma falha transitória aqui pode ser um bug que não
  termina sozinho.
- A entrega continua sendo pelo menos uma vez, com as duas janelas de
  duplicação descritas acima.
- Três conexões AMQP por worker, em vez de uma.
- `payments`, `payment_attempts` e `orders` passam a ter dois caminhos de
  escrita, e uma mudança de schema precisa considerar ambos.

## Referências

- [Stripe — ordem de entrega dos eventos](https://docs.stripe.com/webhooks#event-ordering):
  "A Stripe não garante a entrega dos eventos na ordem em que foram gerados."
- [Stripe — eventos duplicados](https://docs.stripe.com/webhooks#handle-duplicate-events)
- [Stripe — fulfillment e eventos assíncronos](https://docs.stripe.com/checkout/fulfillment)
- [RabbitMQ — consumer acknowledgements e prefetch](https://www.rabbitmq.com/docs/confirms)
- [RabbitMQ — dead letter exchanges](https://www.rabbitmq.com/docs/dlx)
- [RabbitMQ — TTL](https://www.rabbitmq.com/docs/ttl)
- [RabbitMQ — quorum queues](https://www.rabbitmq.com/docs/quorum-queues)
- [PostgreSQL — locking explícito](https://www.postgresql.org/docs/current/explicit-locking.html)
