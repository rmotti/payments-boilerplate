# ADR 0011: contrato de recepção de webhooks e conteúdo da outbox

- Status: aceito
- Data: 2026-09-07
- Implementação: concluída na primeira entrega da Fase 3

## Contexto

A Fase 3 introduz a confirmação assíncrona do pagamento. A API passa a receber
eventos assinados pela Stripe, gravá-los em uma inbox durável e criar, na mesma
transação, a mensagem de outbox que o relay publicará no RabbitMQ.

Duas questões precisam ser fechadas antes do código porque ambas se tornam
contrato. Migrations publicadas são imutáveis por convenção, então a forma da
`outbox_events` é difícil de corrigir depois. E os status HTTP do webhook são
comportamento observável pelo provedor: a Stripe encerra as tentativas apenas
diante de um `2xx` e reentrega diante de qualquer outra resposta, `400`
incluído.

A tabela `webhook_events` já existia desde a Fase 2, com o payload bruto
separado do payload navegável e um índice único sobre
`(provider, provider_event_id)`. A `outbox_events` foi criada por esta decisão,
na migration `00003_outbox.sql`.

## Decisão

### Conteúdo da mensagem de outbox

A mensagem carrega apenas uma referência ao evento, não uma cópia dele. São
publicados `messageId`, `type`, `schemaVersion`, `occurredAt`, `correlationId`
e o identificador do `webhook_event` correspondente. O payload do provedor
permanece exclusivamente na inbox, e o consumer o relê ao processar.

O padrão é seguro neste desenho por causa do próprio outbox: a linha de outbox é
gravada na mesma transação do evento, e o relay só enxerga linhas já commitadas.
Se a mensagem existe, a linha da inbox existe. A referência nunca chega antes do
dado que ela referencia.

### Contrato de resposta do endpoint

A regra que governa todos os casos:

> Responder `2xx` se, e somente se, o evento estiver gravado de forma durável ou
> tiver sido deliberadamente ignorado. Em todos os demais casos, responder
> não-`2xx`, escolhendo o código pelo que ele diz a quem opera a instalação.

O `2xx` é a única resposta que encerra as tentativas da Stripe, e por isso é a
única decisão de verdade: dizer `2xx` sem ter gravado destrói a rede de
proteção que a reentrega representa. A escolha entre os códigos de erro não
muda o comportamento da Stripe, que reentrega em todos eles; ela existe para
separar, no log e no alarme, o que uma reentrega pode resolver do que só uma
intervenção resolve.

| Situação | Status | Inbox | Outbox |
| --- | --- | --- | --- |
| Evento novo com assinatura válida | `202` | grava | grava |
| Evento já recebido | `200` | conflito, não grava | não |
| Tipo de evento não tratado | `200` | grava como `skipped` | não |
| Assinatura ausente, inválida ou fora da janela de tempo | `400` | não | não |
| Corpo maior que o limite do endpoint | `500` | não | não |
| Falha ao gravar no PostgreSQL | `500` | não | não |

O `202` distingue o evento aceito e enfileirado do `200` que representa um
no-op. Para o provedor a diferença é irrelevante, já que todo `2xx` encerra as
tentativas; ela existe para tornar o efeito legível em logs e testes sem
inspecionar o banco.

Eventos de tipo não tratado são gravados como `skipped` em vez de descartados.
O custo é uma tabela que cresce com eventos que nunca serão processados; em
troca, a instalação percebe quando o provedor passa a enviar um tipo novo e
mantém trilha de auditoria completa do que chegou.

O corpo maior que o limite é classificado como falha própria, e não como falha
de assinatura. O sintoma é o mesmo — sem todos os bytes a verificação não passa
— mas a causa é o limite configurado localmente, e uma reentrega passa a
funcionar depois que ele for corrigido, enquanto nenhuma reentrega conserta uma
assinatura forjada.

Como a Stripe reentrega nos dois casos, a diferença entre `500` e `400` aqui
não muda o que ela faz: ela muda o que a instalação vê. Um `500` aponta para
algo que se conserta do lado de cá e que a próxima entrega resolve; um `400`
aponta para uma requisição que nunca vai verificar. Confundir os dois esconde
um limite mal configurado dentro do ruído de assinaturas inválidas. Isso exige
detectar o estouro de tamanho separadamente da falha de verificação.

`401` não é usado. O [ADR 0010](0010-route-access-model.md) o reserva para a
semântica de API key, e o webhook não usa API key. Os casos de `400` recebem o
código de erro estável `invalid_signature`; os de `500` reutilizam
`internal_error`.

O corpo da resposta segue a estrutura de erro comum do contrato, mas serve à
observabilidade da própria instalação: o provedor lê apenas o status.

A transação de gravação usa timeout curto e próprio, menor que o timeout do
provedor. Uma requisição pendurada produz a mesma reentrega que um `500`, porém
sem registro local do que ocorreu.

## Alternativas consideradas

### Publicar o evento completo do provedor na mensagem

Rejeitada. Criaria uma segunda cópia do mesmo dado sem mecanismo de sincronia e
sem regra de precedência. A inbox existe para permitir reprocessamento, e um
snapshot em trânsito no broker produziria dois caminhos de processamento sobre
versões possivelmente diferentes do mesmo evento.

Como redundância a cópia também não se sustenta: a mensagem é apagada no ack,
logo ela cobre apenas a janela em que a linha da inbox comprovadamente existe.
A cópia autoritativa para recuperação é a do próprio provedor, que retém os
eventos e permite reenviar entregas.

A mensagem completa acoplaria ainda o contrato de mensageria ao formato de
evento da Stripe, o que encareceria a adoção de um segundo provedor.

Como efeito secundário, o broker deixa de armazenar o payload e os dados diretos
do cliente. As referências da mensagem ainda podem ser relacionadas à inbox e
continuam protegidas como dados operacionais; o PostgreSQL passa a ser o único
sistema que mantém o evento completo.

### Descartar eventos de tipo não tratado sem gravá-los

Rejeitada. Manteria a tabela menor, mas eliminaria o registro de que o evento
chegou e o sinal de que o provedor começou a enviar um tipo novo.

### Responder `400` para corpo maior que o limite

Rejeitada. Confundiria um limite mal configurado, que a instalação conserta e
cuja próxima entrega passa, com uma requisição que jamais poderá ser
verificada. As duas continuariam sendo reentregues pela Stripe, mas o operador
perderia o sinal que distingue uma da outra.

### Responder sempre `2xx` após verificar a assinatura

Rejeitada. Encerraria as reentregas do provedor mesmo quando a gravação
falhasse, removendo a única rede de proteção contra a perda de um evento já
verificado.

## Consequências

### Positivas

- Existe uma única cópia autoritativa de cada evento recebido.
- O contrato de mensageria nasce neutro em relação ao provedor.
- O broker não armazena dados pessoais nem payloads do provedor.
- Cada status responde à pergunta de se a reentrega pode ter sucesso, o que
  torna o comportamento sob falha previsível e testável.
- A tabela de situações define diretamente os casos de teste da entrega.

### Limitações

- O consumer depende do PostgreSQL para ler o evento antes de processá-lo. A
  dependência não é nova, já que ele precisa do banco para persistir efeitos.
- Uma mensagem na dead-letter queue mostra tipo, horário e correlação, mas não
  o conteúdo do evento; a investigação exige consultar a inbox.
- Gravar eventos ignorados faz a inbox crescer com linhas que nunca produzem
  efeito, e uma política de retenção pode ser necessária depois.
- Um corpo acima do limite permite provocar respostas `5xx` deliberadamente. O
  rate limiting das rotas sujeitas a abuso pertence à Fase 4.
- Uma assinatura inválida causada por segredo mal configurado consome as
  tentativas do provedor; a detecção depende de alarme sobre a métrica de
  assinatura inválida.

## Estado de implementação

A migration `00003_outbox.sql` cria `outbox_events` com a referência ao evento,
a unicidade de uma mensagem por evento recebido e o índice parcial que o relay
usará para varrer apenas linhas não publicadas.

`POST /v1/webhooks/stripe` está no contrato OpenAPI e é a segunda operação
pública do allowlist. O corpo é declarado como binário para que o código gerado
entregue os bytes intocados ao handler; a verificação usa o SDK oficial sobre
esses bytes e ignora divergência de versão de API, porque nada interpreta o
grafo de objetos do provedor nesta etapa.

A inbox e a outbox são gravadas por `sqlc` sobre `database/sql`, em uma
transação própria. O webhook tem limite de corpo separado do limite das rotas
de negócio, e o estouro é detectado antes da verificação para não ser
confundido com uma assinatura inválida.

Os testes cobrem assinatura ausente, forjada e expirada, corpo acima do limite,
preservação dos bytes brutos, evento novo, redelivery sequencial e concorrente,
tipo não tratado, rollback quando a gravação da outbox falha e a ausência de
payload do provedor na mensagem.

Dois testes verificam a unicidade diretamente no schema, inserindo pelo banco
sem passar pelo repository. Eles existem porque o teste de concorrência sozinho
é fraco: sob `READ COMMITTED` o perdedor da corrida bloqueia no índice único até
o vencedor commitar, então até uma implementação que consultasse antes de
inserir passaria. Fixar a garantia no índice impede que a deduplicação migre
silenciosamente para código de aplicação, onde ela perderia corridas que hoje o
banco vence.

O relay, o consumer e a inspeção operacional da inbox foram concluídos nas
entregas seguintes da Fase 3.

## Referências

- [Stripe — verificação de assinatura de webhooks](https://docs.stripe.com/webhooks/signature)
- [Stripe — boas práticas de webhooks](https://docs.stripe.com/webhooks)
- [Transactional outbox](https://microservices.io/patterns/data/transactional-outbox.html)
- [Claim check](https://www.enterpriseintegrationpatterns.com/patterns/messaging/StoreInLibrary.html)
