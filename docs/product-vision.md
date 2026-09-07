# Visão e escopo do produto

## Problema

Integrar pagamentos parece simples no caminho feliz, mas uma implementação real
precisa lidar com timeouts, retries, eventos duplicados, webhooks fora de ordem,
reembolsos, auditoria e proteção de dados. Muitos exemplos ensinam apenas a criar
uma sessão de checkout e deixam essas responsabilidades sem tratamento.

O Payments Boilerplate pretende oferecer uma base pequena e compreensível para
resolver esse problema sem esconder o domínio atrás de abstrações prematuras.

## Proposta de valor

Uma pessoa desenvolvedora deve conseguir clonar o projeto, conectar credenciais
de sandbox, abrir o Swagger UI, executar um pagamento e compreender todo o ciclo
de vida da operação. Ao adaptar a base, ela deve preservar por padrão os
mecanismos que evitam dupla cobrança, estados inconsistentes e confiança
indevida no cliente.

## Público inicial

- Pessoas desenvolvedoras construindo sistemas de comércio eletrônico.
- Equipes que desejam acelerar a integração do seu sistema com um provedor de
  pagamentos.
- Mantenedores que desejam estudar ou comparar integrações de provedores.

O consumidor que realiza a compra não é usuário direto do boilerplate. Ele
interage com o sistema criado pelo integrador e com o checkout hospedado pelo
provedor.

## O que o projeto é

- Uma API headless de referência, copiável e adaptável.
- Uma implementação vertical de pagamento único.
- Um conjunto de padrões para idempotência, webhooks e estados de pagamento.
- Um exemplo de isolamento entre domínio e SDK de um provedor.
- Um contrato OpenAPI acompanhado por Swagger UI, documentação operacional e
  testes dos principais cenários de falha.

## O que o projeto não é

- Gateway, adquirente, subadquirente ou instituição financeira.
- Cofre de cartões ou solução de tokenização própria.
- Motor de antifraude.
- Sistema contábil, fiscal ou de conciliação bancária completa.
- Garantia automática de conformidade PCI DSS ou LGPD.
- Abstração universal para todos os provedores de pagamento.
- Plataforma de marketplace, split ou payout na primeira versão.
- Frontend de loja, carrinho ou painel administrativo.
- SDK de interface visual para o consumidor final.

## Escopo da versão 0.1

### Incluído

- Criação de um pedido com valor e moeda definidos no servidor.
- Início de Stripe Checkout hospedado em modo de pagamento único.
- Pagamento por cartão em BRL; Pix será habilitado depois que o processamento
  assíncrono estiver validado.
- Uso de idempotência na criação de operações remotas.
- Autenticação do backend integrador nas rotas de negócio de uma implantação
  self-hosted e single-integrator.
- Endpoint para consultar o estado atual do pedido.
- Recepção e validação criptográfica de webhooks.
- Inbox persistente para deduplicar, auditar e reprocessar eventos.
- Transactional outbox para publicar eventos no RabbitMQ sem depender de uma
  escrita dupla não atômica.
- Worker concorrente e idempotente para atualizar o estado local de pagamentos.
- Retries com backoff e tratamento de mensagens que esgotarem as tentativas.
- Contrato OpenAPI versionado, Swagger UI e exemplos com `curl`.
- Testes para o caminho feliz e para falhas relevantes.
- Configuração de desenvolvimento e sandbox reproduzível.

### Adiado

- Assinaturas e gestão de acesso por plano.
- Segundo provedor e seleção dinâmica de gateway.
- Reembolso automatizado pela aplicação.
- Cupons, promoções e cálculo de impostos.
- Emissão de documentos fiscais.
- Checkout próprio ou altamente customizado.
- Painel administrativo completo.
- Frontend de demonstração próprio.
- Login e cadastro de consumidores.
- Multi-tenancy e compartilhamento de uma instalação entre integradores.
- Multi-moeda e conversão cambial.
- Chargebacks e automações de disputa.
- Aplicativos móveis.

### Fora dos limites

- Receber, transmitir ou persistir PAN e CVV em sistemas do projeto.
- Guardar chaves secretas no repositório ou expô-las no frontend.
- Marcar um pagamento como concluído apenas por uma chamada originada no cliente
  ou pelo redirecionamento de sucesso do provedor.
- Executar uma cobrança sem proteção contra repetição.
- Alegar certificação ou conformidade regulatória em nome de quem usa o projeto.

## Hipóteses que queremos validar

1. Uma integração completa com um provedor é mais útil que uma camada genérica
   com várias implementações incompletas.
2. Checkout hospedado atende ao primeiro caso de uso e reduz exposição a dados
   de cartão.
3. Uma inbox de webhooks torna falhas e reprocessamentos compreensíveis sem
   exigir uma infraestrutura excessivamente complexa.
4. A separação por casos de uso permite extrair bibliotecas somente quando as
   fronteiras reais estiverem conhecidas.
5. OpenAPI e Swagger UI são suficientes para demonstrar o MVP sem manter um
   frontend que não faz parte da proposta principal.
6. Processamento assíncrono durável deve fazer parte do desenho inicial, e não
   ser adicionado somente quando houver aumento de tráfego.
7. RabbitMQ torna explícitos os limites entre recepção, publicação e consumo de
   eventos que uma integração real precisa tratar.

## Critérios de sucesso da versão 0.1

- Duas requisições equivalentes não produzem duas cobranças.
- Um webhook duplicado não executa efeitos de negócio duas vezes.
- Um evento atrasado não faz um estado final regredir indevidamente.
- Fechar o navegador durante o checkout não perde a confirmação do pagamento.
- Uma falha temporária pode ser observada e reprocessada.
- Indisponibilidade temporária do RabbitMQ não perde um evento já aceito.
- Reiniciar um worker durante o processamento não produz efeitos duplicados.
- Nenhum dado completo de cartão passa pela aplicação.
- Uma nova pessoa sobe a API, cria um pedido e inicia um checkout usando apenas
  o README e o Swagger UI.
- Limites, riscos e responsabilidades do usuário estão documentados.

## Decisões da fundação

- Stripe Checkout é o primeiro provedor e a página hospedada é a experiência de
  pagamento do MVP.
- Cartão em BRL é o primeiro meio de pagamento implementado.
- Pix permanece no escopo da versão `0.1.0`, mas entra depois da validação do
  pipeline assíncrono, pois seu resultado pode ser confirmado posteriormente.
- Stripe Billing, Connect, Elements, assinaturas, marketplace e checkout
  embutido ou customizado estão fora dessa entrega.
