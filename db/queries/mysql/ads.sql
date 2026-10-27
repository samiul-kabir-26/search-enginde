-- name: CreateAdCampaign :exec
INSERT INTO ad_campaigns (id, tenant_id, vendor_id, status, daily_budget_minor, currency, bid_minor, targeting, start_at, end_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAdCampaign :one
SELECT * FROM ad_campaigns WHERE tenant_id = ? AND id = ?;

-- name: ListAdCampaigns :many
SELECT * FROM ad_campaigns WHERE tenant_id = ? ORDER BY id DESC LIMIT ?;

-- name: ListAdCampaignsAfter :many
SELECT * FROM ad_campaigns WHERE tenant_id = ? AND id < ? ORDER BY id DESC LIMIT ?;

-- name: UpdateAdCampaign :execrows
UPDATE ad_campaigns
SET vendor_id = ?, status = ?, daily_budget_minor = ?, currency = ?, bid_minor = ?, targeting = ?, start_at = ?, end_at = ?
WHERE tenant_id = ? AND id = ?;

-- name: DeleteAdCampaign :execrows
DELETE FROM ad_campaigns WHERE tenant_id = ? AND id = ?;

-- name: CreateAdCreative :exec
INSERT INTO ad_creatives (id, campaign_id, product_id)
VALUES (?, ?, ?);

-- name: GetAdCreative :one
SELECT c.* FROM ad_creatives c
JOIN ad_campaigns a ON c.campaign_id = a.id
WHERE a.tenant_id = ? AND c.campaign_id = ? AND c.id = ?;

-- name: ListAdCreatives :many
SELECT c.* FROM ad_creatives c
JOIN ad_campaigns a ON c.campaign_id = a.id
WHERE a.tenant_id = ? AND c.campaign_id = ?
ORDER BY c.id DESC LIMIT ?;

-- name: ListAdCreativesAfter :many
SELECT c.* FROM ad_creatives c
JOIN ad_campaigns a ON c.campaign_id = a.id
WHERE a.tenant_id = ? AND c.campaign_id = ? AND c.id < ?
ORDER BY c.id DESC LIMIT ?;

-- name: DeleteAdCreative :execrows
DELETE c FROM ad_creatives c
JOIN ad_campaigns a ON c.campaign_id = a.id
WHERE a.tenant_id = ? AND c.campaign_id = ? AND c.id = ?;

-- name: ListActiveCampaignCreatives :many
SELECT a.id AS campaign_id, a.tenant_id, a.vendor_id, a.status, a.daily_budget_minor, a.currency, a.bid_minor, a.targeting, a.start_at, a.end_at,
       c.id AS creative_id, c.product_id
FROM ad_campaigns a
JOIN ad_creatives c ON a.id = c.campaign_id
WHERE a.tenant_id = ? AND a.status = 'active';

-- name: ListCreativeCampaigns :many
SELECT c.product_id, c.campaign_id
FROM ad_creatives c
JOIN ad_campaigns a ON c.campaign_id = a.id
WHERE a.tenant_id = ?
ORDER BY c.campaign_id, c.product_id;
