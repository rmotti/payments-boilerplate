# Política de segurança

## Estado do projeto

O Payments Boilerplate ainda está em definição e não possui uma versão suportada
para produção. Correções de segurança são aplicadas na `main`.

## Como reportar uma vulnerabilidade

Use o canal privado do GitHub:
[**Report a vulnerability**](https://github.com/rmotti/payments-boilerplate/security/advisories/new).

O relato fica visível apenas para você e para a manutenção do projeto até que um
advisory seja publicado. Não abra issue pública, pull request ou discussão com
detalhes exploráveis.

Inclua o que for possível: versão ou commit, ambiente, passos de reprodução,
impacto observado e, se houver, um proof of concept mínimo.

Expectativa de resposta: confirmação de recebimento em até 5 dias úteis e uma
posição sobre a validade do relato em até 15 dias úteis. O projeto é mantido em
tempo parcial e não oferece SLA nem recompensa.

## Dados persistidos

**A aplicação armazena dados pessoais.** Cada evento recebido da Stripe é
gravado duas vezes em `webhook_events` — os bytes assinados em `raw_payload` e o
evento parseado em `payload` — e um evento de Checkout carrega nome, e-mail,
telefone e endereços do cliente.

A aplicação não armazena dado completo de cartão, porque o Checkout hospedado
faz com que o número nunca chegue a este código. Isso não torna o restante do
payload inofensivo.

O inventário completo, os prazos de retenção, o procedimento de expurgo e o
modelo de ameaça estão em [`docs/security.md`](docs/security.md). A decisão que
os originou está em
[ADR 0017](docs/decisions/0017-sensitive-data-and-error-handling.md).

Ao reportar um problema ou abrir uma issue, não envie chaves reais, dados
pessoais ou dados de cartão. Use apenas credenciais e cartões de sandbox nos
exemplos reproduzíveis.

## Credenciais

- Trate `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` e `INTEGRATION_API_KEYS`
  como secrets. Nunca os exponha no Swagger UI, em respostas, em logs ou em
  labels de métrica.
- Trate credenciais e certificados do RabbitMQ como secrets, e não exponha a
  interface de administração à internet pública.
- Use conexões protegidas e usuários com o menor conjunto de permissões
  necessário em produção.
- Se um secret for publicado, revogue-o no provedor antes de qualquer outra
  providência e remova-o também do histórico quando aplicável.

## Invariantes de segurança do desenho

Mudanças que quebrem qualquer um destes pontos precisam de decisão registrada:

- `Stripe-Signature` é verificada sobre os bytes originais da requisição antes
  de desserializar ou persistir um evento como válido.
- As mensagens publicadas no RabbitMQ carregam referência ao evento, não uma
  cópia dele. O broker, as filas de retry e a DLQ não contêm dados pessoais.
- As queries da API operacional nunca selecionam `raw_payload` ou `payload`.
- `last_error` é persistido e devolvido pela API operacional. É superfície
  pública: sanitize antes de truncar, e nunca inclua payload, credencial, DSN ou
  resposta integral do provedor.
- Corpos de webhook não vão para logs. Mensagens retidas para retry ou enviadas
  à DLQ continuam sendo dados persistidos e seguem os mesmos critérios de
  minimização aplicados ao banco e aos logs.

## Limites da política

O uso do projeto não certifica uma aplicação como compatível com PCI DSS, LGPD
ou outras normas. Não há isolamento multi-tenant: uma chave de integração vale
para toda a instalação. Não há defesa contra fraude, chargeback ou ataque
volumétrico.

Cada implantação precisa avaliar suas próprias obrigações, provedores,
infraestrutura e processos operacionais. O escopo do que o projeto protege e do
que ele deliberadamente não protege está em
[`docs/security.md`](docs/security.md).
