import {execFileSync} from 'node:child_process';
import {randomBytes} from 'node:crypto';
import {expect,test} from '@playwright/test';
import {uniqueHandle} from './db-fixtures';
import {pgpFixture,seedPGPAccess,signedIn,submitAndLoad} from './db-helpers';
import {waitForStyles} from './styles';

// Seeded financial state is a browser fixture, not proof of payment settlement.
// The actual forms, order isolation, replacement persistence and responsive UI
// run against PostgreSQL, with browser JavaScript disabled.
test('order messages and replacement delivery remain on the correct order',async({browser,baseURL})=>{
 test.setTimeout(90_000);
 const sql=(q:string)=>execFileSync('psql',[process.env.E2E_DATABASE_URL!,'-v','ON_ERROR_STOP=1','-qtAc',q],{encoding:'utf8'}).trim();
 const buyerHandle=uniqueHandle('order_mail_buyer'),vendorHandle=uniqueHandle('order_mail_vendor');
 const buyer=await signedIn(browser,baseURL,buyerHandle,'browser-order-mail-buyer-123',true,320);
 const vendor=await signedIn(browser,baseURL,vendorHandle,'browser-order-mail-vendor-123',true,320);
 const product=randomBytes(16).toString('hex'),order=randomBytes(32).toString('hex'),other=randomBytes(32).toString('hex');
 try {
  await seedPGPAccess(buyer);
  await seedPGPAccess(vendor);
  sql(`UPDATE users SET role='vendor',pgp='${pgpFixture('recipient.pub.asc').trim()}' WHERE handle='${vendorHandle}';
   INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
   SELECT '${product}',id,'Order correspondence fixture','Browser fixture only','Digital','Worldwide','digital',0,500000000000,1 FROM users WHERE handle='${vendorHandle}';
   INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
   SELECT '${order}',id,'${product}','XMR',500000000000,'completed' FROM users WHERE handle='${buyerHandle}';
   INSERT INTO orders(id,buyer_id,product_id,currency,amount,state)
   SELECT '${other}',id,'${product}','XMR',500000000000,'completed' FROM users WHERE handle='${buyerHandle}';
   INSERT INTO deliveries(order_id,content) VALUES('${order}','Original delivery content');`);
  await buyer.goto(`/order?id=${order}`);
  await buyer.locator('form[action="/orders/message"] textarea[name="body"]').fill(pgpFixture('message-to-recipient.asc'));
  await submitAndLoad(buyer,buyer.getByRole('button',{name:'Send order message',exact:true}));
  await expect(buyer).toHaveURL(new RegExp(`/order\\?id=${order}#order-messages$`));
  await expect(buyer.getByRole('heading',{name:'Order correspondence',exact:true})).toBeVisible();
  await expect(buyer.locator('textarea[aria-label^="Order message from"]')).toHaveCount(1);
  await buyer.goto(`/order?id=${other}`);
  await expect(buyer.locator('textarea[aria-label^="Order message from"]')).toHaveCount(0);
  await buyer.goto('/messages');
  await expect(buyer.locator('.messages-list article.message')).toHaveCount(0);
  await vendor.goto(`/order?id=${order}`);
  await expect(vendor.locator('textarea[aria-label^="Order message from"]')).toHaveCount(1);
  await vendor.getByText('Add replacement delivery content',{exact:true}).click();
  await vendor.getByLabel('Replacement content',{exact:true}).fill('Replacement download for this order only');
  await submitAndLoad(vendor,vendor.getByRole('button',{name:'Add delivery update',exact:true}));
  await buyer.goto(`/order?id=${order}`);
  await expect(buyer.getByRole('textbox',{name:'Digital delivery',exact:true})).toHaveValue('Original delivery content');
  await expect(buyer.getByLabel('Replacement delivery content',{exact:true})).toHaveValue(/^-----BEGIN PGP MESSAGE-----/);
  await expect(buyer.getByLabel('Replacement delivery content',{exact:true})).not.toHaveValue(/Replacement download/);
  expect(sql(`SELECT encrypted::text FROM order_delivery_updates WHERE order_id='${order}'`)).toBe('true');
  for(const width of [320,390,768,1280]) {
   await buyer.setViewportSize({width,height:900});await waitForStyles(buyer);
   expect(await buyer.evaluate(()=>document.documentElement.scrollWidth),`order content at ${width}px`).toBeLessThanOrEqual(width);
  }
  await expect(buyer.locator('script')).toHaveCount(0);
  expect(sql(`SELECT count(*) FROM messages WHERE body LIKE '%Replacement download%'`)).toBe('0');
  expect(sql(`SELECT count(*) FROM order_messages WHERE order_id='${order}'`)).toBe('1');
  expect(sql(`SELECT count(*) FROM order_delivery_updates WHERE order_id='${order}'`)).toBe('1');
  expect(sql(`SELECT count(*) FROM notifications WHERE order_id='${order}' AND body LIKE '%Replacement download%'`)).toBe('0');
 } finally {await buyer.context().close();await vendor.context().close();}
});
