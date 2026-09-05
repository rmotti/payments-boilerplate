# Política de segurança

## Estado do projeto

O Payments Boilerplate ainda está em definição e não possui uma versão suportada
para produção.

## Como reportar uma vulnerabilidade

Não publique detalhes exploráveis em uma issue pública. Até que um canal privado
seja configurado, abra uma issue sem informações sensíveis solicitando contato
com a manutenção do projeto.

Antes da primeira versão pública, este processo deverá ser substituído por um
canal privado claramente identificado no repositório.

## Dados e credenciais

- Nunca envie chaves reais, dados pessoais ou dados de cartão em relatórios.
- Use apenas credenciais e cartões de sandbox nos exemplos reproduzíveis.
- Trate `STRIPE_SECRET_KEY` e `STRIPE_WEBHOOK_SECRET` como secrets; nunca os
  exponha no Swagger UI, em respostas ou em logs.
- Verifique `Stripe-Signature` sobre os bytes originais da requisição antes de
  desserializar ou persistir um evento como válido.
- Se um secret for publicado, revogue-o no provedor antes de qualquer outra
  providência e remova-o também do histórico quando aplicável.
- Trate credenciais e certificados do RabbitMQ como secrets.
- Não exponha a interface de administração do RabbitMQ à internet pública.
- Use conexões protegidas e usuários com o menor conjunto de permissões
  necessário em ambientes de produção.
- Não inclua dados completos de cartão em mensagens, filas ou dead-letter
  queues.

Não inclua o corpo completo de webhooks em logs por padrão. Mensagens retidas
para retry ou enviadas à DLQ continuam sendo dados
persistidos. Seu conteúdo e período de retenção devem respeitar os mesmos
critérios de minimização aplicados ao banco e aos logs.


## Limites da política

O uso do projeto não certifica uma aplicação como compatível com PCI DSS, LGPD
ou outras normas. Cada implantação precisa avaliar suas próprias obrigações,
provedores, infraestrutura e processos operacionais.
