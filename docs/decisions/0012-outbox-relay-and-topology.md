# ADR 0012: execução do relay e topologia do RabbitMQ

- Status: aceito
- Data: 2026-09-07
- Implementação: concluída na segunda entrega da Fase 3

## Contexto

O [ADR 0011](0011-webhook-reception-and-outbox.md) fez a API gravar o evento
recebido e sua mensagem de outbox na mesma transação. Falta publicar essas
mensagens no RabbitMQ.

A arquitetura descreve API, relay e consumer como três responsabilidades, mas
não diz em que processo o relay roda, e o guia de implantação prevê apenas dois
serviços. A topologia do broker também estava esboçada com uma routing key que
não sobrevive ao formato real das mensagens.

Ambas as questões precisam ser fechadas antes do código: a primeira determina o
desenho da implantação, e a segunda vira contrato assim que a primeira mensagem
for publicada.

## Decisão

### O relay roda dentro do worker

O relay é executado no processo `worker`, como um componente independente do
consumer. Responsabilidade separada não implica processo separado: `cmd/worker`
é o processo que hospeda trabalho assíncrono, e passa a conter dois componentes
com configuração e ciclo de vida próprios.

```text
cmd/worker
├── outbox relay -> PostgreSQL -> RabbitMQ
└── consumer     -> RabbitMQ -> regras de negócio -> PostgreSQL
```

A implantação continua com `api` e `worker`, sem terceiro serviço.

A implementação precisa admitir mais de uma instância de worker com segurança.
O mecanismo é um *lease*, não um lock mantido durante a publicação:

```text
transação curta:
  seleciona mensagens vencidas com FOR UPDATE SKIP LOCKED
  marca status publishing, grava locked_until e locked_by
commit

fora de transação:
  publica e aguarda o publisher confirm

transação curta:
  marca published, ou reagenda next_attempt_at com backoff
```

Nenhuma transação fica aberta durante I/O de rede. Isso importa porque a
alternativa — segurar o lock do lote inteiro enquanto se publica — acopla a
duração do lock à saúde do broker e cria uma incoerência aritmética: um lote de
50 mensagens com 5 s de orçamento de confirm cada precisaria de até 250 s, mas a
transação expiraria antes, revertendo marcações de mensagens que o broker já
havia confirmado e republicando todas elas.

Enquanto o lease vale, outra instância não recebe a mensagem. Não há renovação:
quando o prazo passa sem que a mensagem tenha sido liquidada, ela volta ao
conjunto de trabalho. É assim que um relay que morreu no meio de uma publicação
devolve o que estava fazendo, sem coordenação externa.

Todos os instantes persistidos do lease são calculados pelo PostgreSQL, com
`now()`, e nunca pelos processos. Com várias instâncias, o banco é o único
relógio que elas compartilham; se cada uma calculasse prazos pelo próprio
relógio, uma máquina adiantada declararia vencido o lease de outra que ainda
está publicando.

O dono do lease identifica o **processo**, não a máquina: é o hostname mais um
sufixo aleatório gerado na inicialização. Dois workers no mesmo host teriam o
mesmo hostname, e o mais antigo poderia liquidar uma mensagem que o outro já
havia assumido.

A liquidação exige que o lease ainda seja nosso e ainda esteja vigente. Quando
não é o caso, nenhuma linha é atualizada, e isso é reportado como lease perdido
em vez de sucesso: a publicação pode ter acontecido, mas nada a registrou, e a
mensagem será publicada de novo. Contabilizá-la como publicada faria métricas e
logs mentirem justamente no caso que mais importa investigar.

Isso não elimina a duplicação. Uma queda entre o confirm do broker e a gravação
de `published` republica a mensagem, o que é inerente à entrega pelo menos uma
vez. A liquidação é escopada ao dono do lease, de modo que um relay cujo lease
expirou não sobrescreve o trabalho de quem assumiu a mensagem.

A configuração recusa iniciar quando `OUTBOX_LEASE_DURATION` for curto demais
para `OUTBOX_BATCH_SIZE` mais a reserva de liquidação, para que a incoerência
acima não possa ser reintroduzida por configuração. Esse cálculo só é válido
porque cada tentativa de publicação tem teto: um deadline permanece instalado
no socket durante rediscagem, handshake AMQP, abertura do canal, declaração da
topologia, envio e espera do confirm. Isso é necessário porque o cancelamento
do `context` do driver, sozinho, não interrompe todas as operações síncronas.

O processo não usa o timestamp absoluto devolvido pelo PostgreSQL como deadline
local, pois isso voltaria a comparar relógios de máquinas diferentes. Em vez
disso, inicia uma janela monotônica conservadora **antes** de pedir o lease ao
banco e a encerra com antecedência suficiente para liquidar a tentativa. Como o
banco cria o lease depois do início dessa janela, a publicação termina antes do
vencimento persistido independentemente de diferença entre relógios.

Se um dia houver necessidade de escalar, monitorar ou implantar o relay
separadamente, ele poderá ser extraído para `cmd/relay` sem que sua regra de
negócio mude.

### Topologia

```text
payments.events (topic, durável)
  |
  +-- payment.webhook.#  -> payments.webhooks (durável)
                              |
                              +-- rejeição -> payments.webhooks.dlx
                                                |
                                                `-> payments.webhooks.dlq
```

A routing key de binding é `payment.webhook.#`, e não `payment.webhook.*`. As
mensagens usam o tipo de domínio na chave, como
`payment.webhook.checkout.completed`, e em uma exchange topic o `*` casa
exatamente uma palavra entre pontos, de modo que `*` deixaria de fora toda chave
com mais de um segmento após o prefixo. O `#` casa qualquer quantidade.

A dead-letter é declarada agora, junto do resto da topologia, mesmo que só passe
a receber mensagens quando o consumer existir. Declarar a fila principal sem seu
`x-dead-letter-exchange` obrigaria a recriá-la depois, e no RabbitMQ os
argumentos de uma fila são imutáveis: mudá-los exige apagar a fila, o que não é
uma operação aceitável sobre uma fila que já carrega mensagens financeiras.

A topologia é declarada pela aplicação de forma idempotente, na inicialização do
worker, e todas as suas partes são duráveis. As mensagens são publicadas com
`DeliveryMode` persistente.

### Publicação e marcação

Uma mensagem só é marcada como publicada depois do publisher confirm do broker.
Confirmações são aguardadas com deadline; um confirm negativo ou expirado deixa
a linha pendente para o próximo ciclo, com o contador de tentativas incrementado
e o erro registrado.

### Política de falhas

Falhas são classificadas, e a classificação decide o destino da mensagem.

Indisponibilidade do broker, conexão fechada, timeout, confirm negativo e
mensagem não roteada são **transitórias**. Elas nunca abandonam a mensagem, por
mais que se repitam: são condições que terminam sozinhas, e descartar um evento
financeiro porque a infraestrutura passou um mau minuto é inaceitável. Cada
falha reagenda a mensagem com backoff exponencial e jitter — o jitter evita que
tudo que falhou durante uma indisponibilidade vença no mesmo instante e atropele
o broker assim que ele volta.

Apenas erros classificados como **permanentes** levam a `failed`. Hoje isso
significa uma mensagem que não pode sequer ser serializada. O padrão é
transitório: um erro só é permanente quando o código diz que é.

`OUTBOX_ALERT_AFTER_ATTEMPTS` controla quando uma mensagem que insiste em falhar
é reportada, e **não** interrompe as tentativas. A contagem serve ao operador,
não à decisão de desistir.

Um limite terminal por tempo pode ser considerado depois. A inspeção e o
reprocessamento seguro já existem por meio das operações autenticadas de
webhook; uma mensagem em `failed` pode ser diagnosticada e sua inbox/outbox
original reenfileirada sem criar uma segunda cópia.

A ordem de publicação é por antiguidade, mas não há garantia de ordenação entre
mensagens: o consumer precisa tolerar eventos fora de ordem de qualquer forma,
porque o provedor também não garante ordem.

## Alternativas consideradas

### Um terceiro binário `cmd/relay`

Rejeitada por ora. Daria escala e falha independentes, mas exigiria um terceiro
serviço na Railway, entradas adicionais no compose e no guia de implantação, e
não resolve nenhum problema concreto na escala atual do projeto. A extração
continua possível sem mudança de regra de negócio.

### Publicar a partir da API

Rejeitada. Acoplaria a latência da API ao broker e faria o trabalho de fundo
escalar junto com o tráfego HTTP, desfazendo a separação entre receber e agir
que motivou o outbox.

### Publicar dentro da transação que grava o evento

Rejeitada, e é justamente o problema que o outbox existe para resolver.
PostgreSQL e RabbitMQ não compartilham transação, então publicar antes do commit
pode anunciar um evento que não existe, e publicar depois abre uma janela em que
o evento existe e nunca será publicado.

### Notificar o relay com `LISTEN`/`NOTIFY` em vez de polling

Adiada. Reduziria a latência entre gravar e publicar, mas adiciona um caminho de
notificação que precisa de fallback por polling mesmo assim, já que uma
notificação perdida não pode deixar a mensagem parada para sempre. O polling
sozinho é suficiente para as metas de latência registradas em
[métricas](../metrics.md), e a otimização pode vir depois com medição.

### Marcar como publicada antes do confirm

Rejeitada. O confirm é o que distingue uma mensagem aceita pelo broker de uma
perdida em um canal que caiu. Marcar antes reintroduz exatamente a perda que o
outbox existe para impedir.

### Segurar a transação e os locks durante a publicação

Rejeitada, e foi o desenho da primeira versão desta entrega. Ela dispensa as
colunas de lease e torna impossível que duas instâncias publiquem
concorrentemente, mas mantém uma transação aberta durante I/O de rede: os locks
duram o que o broker demorar, e um lote que ultrapasse o timeout da transação
reverte marcações de mensagens já confirmadas, republicando-as. O lease custa
três colunas e entrega o mesmo isolamento sem prender o banco a um sistema
externo.

### Confiar apenas no publisher confirm, sem `mandatory`

Rejeitada. Um confirm prova que a exchange aceitou a mensagem, não que alguma
fila a recebeu: uma routing key sem binding é confirmada e descartada em
silêncio, e o relay a marcaria como publicada. Com publicação `mandatory`, a
mensagem não roteada retorna antes do confirm e é tratada como falha
transitória, já que um binding pode ser restaurado.

## Consequências

### Positivas

- A implantação continua com dois serviços.
- Mais de uma instância de worker é segura por construção, sem coordenação
  externa nem lock distribuído.
- Nenhuma transação fica aberta durante chamadas ao broker.
- Nenhuma mensagem é marcada como publicada sem confirmação do broker, e uma
  mensagem que nenhuma fila recebeu não conta como publicada.
- Uma indisponibilidade do broker, por mais longa que seja, não descarta
  eventos: só um erro classificado como permanente faz isso.
- Um relay que morre devolve seu trabalho quando o lease expira.
- A dead-letter existe desde a primeira declaração, evitando recriar a fila.
- Relay e consumer têm configuração e ciclo próprios, o que mantém a extração
  futura barata.

### Limitações

- Relay e consumer escalam juntos, porque compartilham o processo.
- O polling impõe uma latência mínima igual ao seu intervalo, mesmo quando a
  fila está vazia e a mensagem acabou de ser gravada.
- Uma mensagem em `failed` exige intervenção: nada a reprocessa
  automaticamente, e a inspeção operacional pertence a uma entrega posterior.
- A entrega continua sendo pelo menos uma vez. Uma queda entre o confirm e a
  marcação republica a mensagem, e o consumer precisa ser idempotente.
- Um lease perdido significa trabalho refeito: a mensagem é publicada
  novamente por outra instância, o que é seguro mas indica que a publicação
  está mais lenta que o lease configurado.
- Uma mensagem transitoriamente irrecuperável é tentada indefinidamente. A
  escolha é deliberada — perder um evento é pior que insistir —, mas depende de
  alguém observar o alerta de mensagem travada.
- A classificação de erros é conservadora: quase tudo é transitório. Ampliá-la
  exige entender bem cada condição, porque classificar algo como permanente por
  engano descarta um evento.

## Referências

- [RabbitMQ — Publisher Confirms](https://www.rabbitmq.com/docs/confirms)
- [RabbitMQ — Topic exchange e routing keys](https://www.rabbitmq.com/tutorials/tutorial-five-go)
- [RabbitMQ — Dead Letter Exchanges](https://www.rabbitmq.com/docs/dlx)
- [PostgreSQL — SELECT FOR UPDATE SKIP LOCKED](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
