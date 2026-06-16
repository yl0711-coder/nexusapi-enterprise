-- MySQL dump 10.13  Distrib 8.0.46, for Linux (x86_64)
--
-- Host: localhost    Database: newapi
-- ------------------------------------------------------
-- Server version	8.0.46

/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
/*!50503 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;

--
-- Current Database: `newapi`
--

CREATE DATABASE /*!32312 IF NOT EXISTS*/ `newapi` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci */ /*!80016 DEFAULT ENCRYPTION='N' */;

USE `newapi`;

--
-- Table structure for table `abilities`
--

DROP TABLE IF EXISTS `abilities`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `abilities` (
  `group` varchar(64) NOT NULL,
  `model` varchar(255) NOT NULL,
  `channel_id` bigint NOT NULL,
  `enabled` tinyint(1) DEFAULT NULL,
  `priority` bigint DEFAULT '0',
  `weight` bigint unsigned DEFAULT '0',
  `tag` varchar(191) DEFAULT NULL,
  PRIMARY KEY (`group`,`model`,`channel_id`),
  KEY `idx_abilities_channel_id` (`channel_id`),
  KEY `idx_abilities_priority` (`priority`),
  KEY `idx_abilities_weight` (`weight`),
  KEY `idx_abilities_tag` (`tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `abilities`
--

LOCK TABLES `abilities` WRITE;
/*!40000 ALTER TABLE `abilities` DISABLE KEYS */;
INSERT INTO `abilities` VALUES ('default','claude-haiku-4-5-20251001',2,1,0,0,''),('default','claude-opus-4-6',2,1,0,0,''),('default','claude-sonnet-4-5-20250929',2,1,0,0,''),('default','deepseek-chat',1,1,0,0,''),('default','glm-4-plus',1,1,0,0,''),('default','gpt-4o-mini',1,1,0,0,''),('default','gpt-5-mini',1,1,0,0,''),('default','qwen-plus',1,1,0,0,''),('enterprise','claude-haiku-4-5-20251001',2,1,0,0,''),('enterprise','claude-opus-4-6',2,1,0,0,''),('enterprise','claude-sonnet-4-5-20250929',2,1,0,0,''),('enterprise','deepseek-chat',1,1,0,0,''),('enterprise','glm-4-plus',1,1,0,0,''),('enterprise','gpt-4o-mini',1,1,0,0,''),('enterprise','gpt-5-mini',1,1,0,0,''),('enterprise','qwen-plus',1,1,0,0,''),('vip','claude-haiku-4-5-20251001',2,1,0,0,''),('vip','claude-opus-4-6',2,1,0,0,''),('vip','claude-sonnet-4-5-20250929',2,1,0,0,''),('vip','deepseek-chat',1,1,0,0,''),('vip','glm-4-plus',1,1,0,0,''),('vip','gpt-4o-mini',1,1,0,0,''),('vip','gpt-5-mini',1,1,0,0,''),('vip','qwen-plus',1,1,0,0,'');
/*!40000 ALTER TABLE `abilities` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `channels`
--

DROP TABLE IF EXISTS `channels`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `channels` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `type` bigint DEFAULT '0',
  `key` longtext NOT NULL,
  `open_ai_organization` longtext,
  `test_model` longtext,
  `status` bigint DEFAULT '1',
  `name` varchar(191) DEFAULT NULL,
  `weight` bigint unsigned DEFAULT '0',
  `created_time` bigint DEFAULT NULL,
  `test_time` bigint DEFAULT NULL,
  `response_time` bigint DEFAULT NULL,
  `base_url` varchar(191) DEFAULT '',
  `other` longtext,
  `balance` double DEFAULT NULL,
  `balance_updated_time` bigint DEFAULT NULL,
  `models` longtext,
  `group` varchar(64) DEFAULT 'default',
  `used_quota` bigint DEFAULT '0',
  `model_mapping` text,
  `status_code_mapping` varchar(1024) DEFAULT '',
  `priority` bigint DEFAULT '0',
  `auto_ban` bigint DEFAULT '1',
  `other_info` longtext,
  `tag` varchar(191) DEFAULT NULL,
  `setting` text,
  `param_override` text,
  `header_override` text,
  `remark` varchar(255) DEFAULT NULL,
  `channel_info` json DEFAULT NULL,
  `settings` longtext,
  PRIMARY KEY (`id`),
  KEY `idx_channels_name` (`name`),
  KEY `idx_channels_tag` (`tag`)
) ENGINE=InnoDB AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `channels`
--

LOCK TABLES `channels` WRITE;
/*!40000 ALTER TABLE `channels` DISABLE KEYS */;
INSERT INTO `channels` VALUES (1,1,'sk-mock-openai-DONOTUSE-0001',NULL,NULL,1,'Mock-OpenAI兼容',0,1781621586,0,0,'https://mock-upstream.local','',0,0,'gpt-5-mini,gpt-4o-mini,deepseek-chat,glm-4-plus,qwen-plus','default,vip,enterprise',0,'','',0,1,'','','',NULL,NULL,NULL,'{\"is_multi_key\": false, \"multi_key_mode\": \"\", \"multi_key_size\": 0, \"multi_key_status_list\": null, \"multi_key_polling_index\": 0}',''),(2,14,'sk-mock-anthropic-DONOTUSE-0001',NULL,NULL,1,'Mock-Anthropic',0,1781621586,0,0,'','',0,0,'claude-opus-4-6,claude-sonnet-4-5-20250929,claude-haiku-4-5-20251001','default,vip,enterprise',0,'','',0,1,'','','',NULL,NULL,NULL,'{\"is_multi_key\": false, \"multi_key_mode\": \"\", \"multi_key_size\": 0, \"multi_key_status_list\": null, \"multi_key_polling_index\": 0}','');
/*!40000 ALTER TABLE `channels` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `checkins`
--

DROP TABLE IF EXISTS `checkins`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `checkins` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint NOT NULL,
  `checkin_date` varchar(10) NOT NULL,
  `quota_awarded` bigint NOT NULL,
  `created_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_user_checkin_date` (`user_id`,`checkin_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `checkins`
--

LOCK TABLES `checkins` WRITE;
/*!40000 ALTER TABLE `checkins` DISABLE KEYS */;
/*!40000 ALTER TABLE `checkins` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `custom_oauth_providers`
--

DROP TABLE IF EXISTS `custom_oauth_providers`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `custom_oauth_providers` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `name` varchar(64) NOT NULL,
  `slug` varchar(64) NOT NULL,
  `icon` varchar(128) DEFAULT '',
  `enabled` tinyint(1) DEFAULT '0',
  `client_id` varchar(256) DEFAULT NULL,
  `client_secret` varchar(512) DEFAULT NULL,
  `authorization_endpoint` varchar(512) DEFAULT NULL,
  `token_endpoint` varchar(512) DEFAULT NULL,
  `user_info_endpoint` varchar(512) DEFAULT NULL,
  `scopes` varchar(256) DEFAULT 'openid profile email',
  `user_id_field` varchar(128) DEFAULT 'sub',
  `username_field` varchar(128) DEFAULT 'preferred_username',
  `display_name_field` varchar(128) DEFAULT 'name',
  `email_field` varchar(128) DEFAULT 'email',
  `well_known` varchar(512) DEFAULT NULL,
  `auth_style` bigint DEFAULT '0',
  `access_policy` text,
  `access_denied_message` varchar(512) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_custom_oauth_providers_slug` (`slug`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `custom_oauth_providers`
--

LOCK TABLES `custom_oauth_providers` WRITE;
/*!40000 ALTER TABLE `custom_oauth_providers` DISABLE KEYS */;
/*!40000 ALTER TABLE `custom_oauth_providers` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `logs`
--

DROP TABLE IF EXISTS `logs`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `logs` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `created_at` bigint DEFAULT NULL,
  `type` bigint DEFAULT NULL,
  `content` longtext,
  `username` varchar(191) DEFAULT '',
  `token_name` varchar(191) DEFAULT '',
  `model_name` varchar(191) DEFAULT '',
  `quota` bigint DEFAULT '0',
  `prompt_tokens` bigint DEFAULT '0',
  `completion_tokens` bigint DEFAULT '0',
  `use_time` bigint DEFAULT '0',
  `is_stream` tinyint(1) DEFAULT NULL,
  `channel_id` bigint DEFAULT NULL,
  `channel_name` longtext,
  `token_id` bigint DEFAULT '0',
  `group` varchar(191) DEFAULT NULL,
  `ip` varchar(191) DEFAULT '',
  `request_id` varchar(64) DEFAULT '',
  `other` longtext,
  PRIMARY KEY (`id`),
  KEY `idx_logs_token_name` (`token_name`),
  KEY `idx_logs_model_name` (`model_name`),
  KEY `idx_logs_channel_id` (`channel_id`),
  KEY `idx_logs_token_id` (`token_id`),
  KEY `idx_created_at_type` (`created_at`,`type`),
  KEY `idx_logs_username` (`username`),
  KEY `idx_logs_group` (`group`),
  KEY `idx_logs_ip` (`ip`),
  KEY `idx_logs_request_id` (`request_id`),
  KEY `idx_created_at_id` (`id`,`created_at`),
  KEY `idx_user_id_id` (`user_id`,`id`),
  KEY `idx_logs_user_id` (`user_id`),
  KEY `index_username_model_name` (`model_name`,`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `logs`
--

LOCK TABLES `logs` WRITE;
/*!40000 ALTER TABLE `logs` DISABLE KEYS */;
/*!40000 ALTER TABLE `logs` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `midjourneys`
--

DROP TABLE IF EXISTS `midjourneys`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `midjourneys` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `code` bigint DEFAULT NULL,
  `user_id` bigint DEFAULT NULL,
  `action` varchar(40) DEFAULT NULL,
  `mj_id` varchar(191) DEFAULT NULL,
  `prompt` longtext,
  `prompt_en` longtext,
  `description` longtext,
  `state` longtext,
  `submit_time` bigint DEFAULT NULL,
  `start_time` bigint DEFAULT NULL,
  `finish_time` bigint DEFAULT NULL,
  `image_url` longtext,
  `video_url` longtext,
  `video_urls` longtext,
  `status` varchar(20) DEFAULT NULL,
  `progress` varchar(30) DEFAULT NULL,
  `fail_reason` longtext,
  `channel_id` bigint DEFAULT NULL,
  `quota` bigint DEFAULT NULL,
  `buttons` longtext,
  `properties` longtext,
  PRIMARY KEY (`id`),
  KEY `idx_midjourneys_status` (`status`),
  KEY `idx_midjourneys_progress` (`progress`),
  KEY `idx_midjourneys_user_id` (`user_id`),
  KEY `idx_midjourneys_action` (`action`),
  KEY `idx_midjourneys_mj_id` (`mj_id`),
  KEY `idx_midjourneys_submit_time` (`submit_time`),
  KEY `idx_midjourneys_start_time` (`start_time`),
  KEY `idx_midjourneys_finish_time` (`finish_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `midjourneys`
--

LOCK TABLES `midjourneys` WRITE;
/*!40000 ALTER TABLE `midjourneys` DISABLE KEYS */;
/*!40000 ALTER TABLE `midjourneys` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `models`
--

DROP TABLE IF EXISTS `models`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `models` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `model_name` varchar(128) NOT NULL,
  `description` text,
  `icon` varchar(128) DEFAULT NULL,
  `tags` varchar(255) DEFAULT NULL,
  `vendor_id` bigint DEFAULT NULL,
  `endpoints` text,
  `status` bigint DEFAULT '1',
  `sync_official` bigint DEFAULT '1',
  `created_time` bigint DEFAULT NULL,
  `updated_time` bigint DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  `name_rule` bigint DEFAULT '0',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_model_name_delete_at` (`model_name`,`deleted_at`),
  KEY `idx_models_deleted_at` (`deleted_at`),
  KEY `idx_models_vendor_id` (`vendor_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `models`
--

LOCK TABLES `models` WRITE;
/*!40000 ALTER TABLE `models` DISABLE KEYS */;
/*!40000 ALTER TABLE `models` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `options`
--

DROP TABLE IF EXISTS `options`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `options` (
  `key` varchar(191) NOT NULL,
  `value` longtext,
  PRIMARY KEY (`key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `options`
--

LOCK TABLES `options` WRITE;
/*!40000 ALTER TABLE `options` DISABLE KEYS */;
INSERT INTO `options` VALUES ('DemoSiteEnabled','false'),('GroupRatio','{\"default\":1,\"vip\":0.9,\"enterprise\":0.85}'),('QuotaPerUnit','500000'),('SelfUseModeEnabled','false');
/*!40000 ALTER TABLE `options` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `passkey_credentials`
--

DROP TABLE IF EXISTS `passkey_credentials`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `passkey_credentials` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint NOT NULL,
  `credential_id` varchar(512) NOT NULL,
  `public_key` text NOT NULL,
  `attestation_type` varchar(255) DEFAULT NULL,
  `aa_guid` varchar(512) DEFAULT NULL,
  `sign_count` int unsigned DEFAULT '0',
  `clone_warning` tinyint(1) DEFAULT NULL,
  `user_present` tinyint(1) DEFAULT NULL,
  `user_verified` tinyint(1) DEFAULT NULL,
  `backup_eligible` tinyint(1) DEFAULT NULL,
  `backup_state` tinyint(1) DEFAULT NULL,
  `transports` text,
  `attachment` varchar(32) DEFAULT NULL,
  `last_used_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_passkey_credentials_credential_id` (`credential_id`),
  UNIQUE KEY `idx_passkey_credentials_user_id` (`user_id`),
  KEY `idx_passkey_credentials_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `passkey_credentials`
--

LOCK TABLES `passkey_credentials` WRITE;
/*!40000 ALTER TABLE `passkey_credentials` DISABLE KEYS */;
/*!40000 ALTER TABLE `passkey_credentials` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `perf_metrics`
--

DROP TABLE IF EXISTS `perf_metrics`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `perf_metrics` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `model_name` varchar(128) DEFAULT NULL,
  `group` varchar(64) DEFAULT NULL,
  `bucket_ts` bigint DEFAULT NULL,
  `request_count` bigint DEFAULT '0',
  `success_count` bigint DEFAULT '0',
  `total_latency_ms` bigint DEFAULT '0',
  `ttft_sum_ms` bigint DEFAULT '0',
  `ttft_count` bigint DEFAULT '0',
  `output_tokens` bigint DEFAULT '0',
  `generation_ms` bigint DEFAULT '0',
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_perf_model_group_bucket` (`model_name`,`group`,`bucket_ts`),
  KEY `idx_perf_bucket_ts` (`bucket_ts`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `perf_metrics`
--

LOCK TABLES `perf_metrics` WRITE;
/*!40000 ALTER TABLE `perf_metrics` DISABLE KEYS */;
/*!40000 ALTER TABLE `perf_metrics` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `prefill_groups`
--

DROP TABLE IF EXISTS `prefill_groups`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `prefill_groups` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `name` varchar(64) NOT NULL,
  `type` varchar(32) NOT NULL,
  `items` json DEFAULT NULL,
  `description` varchar(255) DEFAULT NULL,
  `created_time` bigint DEFAULT NULL,
  `updated_time` bigint DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_prefill_name` (`name`),
  KEY `idx_prefill_groups_type` (`type`),
  KEY `idx_prefill_groups_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `prefill_groups`
--

LOCK TABLES `prefill_groups` WRITE;
/*!40000 ALTER TABLE `prefill_groups` DISABLE KEYS */;
/*!40000 ALTER TABLE `prefill_groups` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `quota_data`
--

DROP TABLE IF EXISTS `quota_data`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `quota_data` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `username` varchar(64) DEFAULT '',
  `model_name` varchar(64) DEFAULT '',
  `created_at` bigint DEFAULT NULL,
  `token_used` bigint DEFAULT '0',
  `count` bigint DEFAULT '0',
  `quota` bigint DEFAULT '0',
  PRIMARY KEY (`id`),
  KEY `idx_quota_data_user_id` (`user_id`),
  KEY `idx_qdt_model_user_name` (`model_name`,`username`),
  KEY `idx_qdt_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `quota_data`
--

LOCK TABLES `quota_data` WRITE;
/*!40000 ALTER TABLE `quota_data` DISABLE KEYS */;
/*!40000 ALTER TABLE `quota_data` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `redemptions`
--

DROP TABLE IF EXISTS `redemptions`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `redemptions` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `key` char(32) DEFAULT NULL,
  `status` bigint DEFAULT '1',
  `name` varchar(191) DEFAULT NULL,
  `quota` bigint DEFAULT '100',
  `created_time` bigint DEFAULT NULL,
  `redeemed_time` bigint DEFAULT NULL,
  `used_user_id` bigint DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  `expired_time` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_redemptions_key` (`key`),
  KEY `idx_redemptions_name` (`name`),
  KEY `idx_redemptions_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `redemptions`
--

LOCK TABLES `redemptions` WRITE;
/*!40000 ALTER TABLE `redemptions` DISABLE KEYS */;
/*!40000 ALTER TABLE `redemptions` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `setups`
--

DROP TABLE IF EXISTS `setups`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `setups` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `version` varchar(50) NOT NULL,
  `initialized_at` bigint NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `setups`
--

LOCK TABLES `setups` WRITE;
/*!40000 ALTER TABLE `setups` DISABLE KEYS */;
INSERT INTO `setups` VALUES (1,'v1.0.0-rc.4',1781621585);
/*!40000 ALTER TABLE `setups` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `subscription_orders`
--

DROP TABLE IF EXISTS `subscription_orders`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `subscription_orders` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `plan_id` bigint DEFAULT NULL,
  `money` double DEFAULT NULL,
  `trade_no` varchar(255) DEFAULT NULL,
  `payment_method` varchar(50) DEFAULT NULL,
  `payment_provider` varchar(50) DEFAULT '',
  `status` longtext,
  `create_time` bigint DEFAULT NULL,
  `complete_time` bigint DEFAULT NULL,
  `provider_payload` text,
  PRIMARY KEY (`id`),
  UNIQUE KEY `trade_no` (`trade_no`),
  KEY `idx_subscription_orders_user_id` (`user_id`),
  KEY `idx_subscription_orders_plan_id` (`plan_id`),
  KEY `idx_subscription_orders_trade_no` (`trade_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `subscription_orders`
--

LOCK TABLES `subscription_orders` WRITE;
/*!40000 ALTER TABLE `subscription_orders` DISABLE KEYS */;
/*!40000 ALTER TABLE `subscription_orders` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `subscription_plans`
--

DROP TABLE IF EXISTS `subscription_plans`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `subscription_plans` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `title` varchar(128) NOT NULL,
  `subtitle` varchar(255) DEFAULT '',
  `price_amount` decimal(10,6) NOT NULL DEFAULT '0.000000',
  `currency` varchar(8) NOT NULL DEFAULT 'USD',
  `duration_unit` varchar(16) NOT NULL DEFAULT 'month',
  `duration_value` bigint NOT NULL DEFAULT '1',
  `custom_seconds` bigint NOT NULL DEFAULT '0',
  `enabled` tinyint(1) DEFAULT '1',
  `sort_order` bigint DEFAULT '0',
  `stripe_price_id` varchar(128) DEFAULT '',
  `creem_product_id` varchar(128) DEFAULT '',
  `max_purchase_per_user` bigint DEFAULT '0',
  `upgrade_group` varchar(64) DEFAULT '',
  `total_amount` bigint NOT NULL DEFAULT '0',
  `quota_reset_period` varchar(16) DEFAULT 'never',
  `quota_reset_custom_seconds` bigint DEFAULT '0',
  `created_at` bigint DEFAULT NULL,
  `updated_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `subscription_plans`
--

LOCK TABLES `subscription_plans` WRITE;
/*!40000 ALTER TABLE `subscription_plans` DISABLE KEYS */;
/*!40000 ALTER TABLE `subscription_plans` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `subscription_pre_consume_records`
--

DROP TABLE IF EXISTS `subscription_pre_consume_records`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `subscription_pre_consume_records` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `request_id` varchar(64) DEFAULT NULL,
  `user_id` bigint DEFAULT NULL,
  `user_subscription_id` bigint DEFAULT NULL,
  `pre_consumed` bigint NOT NULL DEFAULT '0',
  `status` varchar(32) DEFAULT NULL,
  `created_at` bigint DEFAULT NULL,
  `updated_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_subscription_pre_consume_records_request_id` (`request_id`),
  KEY `idx_subscription_pre_consume_records_status` (`status`),
  KEY `idx_subscription_pre_consume_records_updated_at` (`updated_at`),
  KEY `idx_subscription_pre_consume_records_user_id` (`user_id`),
  KEY `idx_subscription_pre_consume_records_user_subscription_id` (`user_subscription_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `subscription_pre_consume_records`
--

LOCK TABLES `subscription_pre_consume_records` WRITE;
/*!40000 ALTER TABLE `subscription_pre_consume_records` DISABLE KEYS */;
/*!40000 ALTER TABLE `subscription_pre_consume_records` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `tasks`
--

DROP TABLE IF EXISTS `tasks`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `tasks` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `created_at` bigint DEFAULT NULL,
  `updated_at` bigint DEFAULT NULL,
  `task_id` varchar(191) DEFAULT NULL,
  `platform` varchar(30) DEFAULT NULL,
  `user_id` bigint DEFAULT NULL,
  `group` varchar(50) DEFAULT NULL,
  `channel_id` bigint DEFAULT NULL,
  `quota` bigint DEFAULT NULL,
  `action` varchar(40) DEFAULT NULL,
  `status` varchar(20) DEFAULT NULL,
  `fail_reason` longtext,
  `submit_time` bigint DEFAULT NULL,
  `start_time` bigint DEFAULT NULL,
  `finish_time` bigint DEFAULT NULL,
  `progress` varchar(20) DEFAULT NULL,
  `properties` json DEFAULT NULL,
  `private_data` json DEFAULT NULL,
  `data` json DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_tasks_finish_time` (`finish_time`),
  KEY `idx_tasks_progress` (`progress`),
  KEY `idx_tasks_created_at` (`created_at`),
  KEY `idx_tasks_task_id` (`task_id`),
  KEY `idx_tasks_platform` (`platform`),
  KEY `idx_tasks_user_id` (`user_id`),
  KEY `idx_tasks_status` (`status`),
  KEY `idx_tasks_channel_id` (`channel_id`),
  KEY `idx_tasks_action` (`action`),
  KEY `idx_tasks_submit_time` (`submit_time`),
  KEY `idx_tasks_start_time` (`start_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `tasks`
--

LOCK TABLES `tasks` WRITE;
/*!40000 ALTER TABLE `tasks` DISABLE KEYS */;
/*!40000 ALTER TABLE `tasks` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `tokens`
--

DROP TABLE IF EXISTS `tokens`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `tokens` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `key` varchar(128) DEFAULT NULL,
  `status` bigint DEFAULT '1',
  `name` varchar(191) DEFAULT NULL,
  `created_time` bigint DEFAULT NULL,
  `accessed_time` bigint DEFAULT NULL,
  `expired_time` bigint DEFAULT '-1',
  `remain_quota` bigint DEFAULT '0',
  `unlimited_quota` tinyint(1) DEFAULT NULL,
  `model_limits_enabled` tinyint(1) DEFAULT NULL,
  `model_limits` text,
  `allow_ips` varchar(191) DEFAULT '',
  `used_quota` bigint DEFAULT '0',
  `group` varchar(191) DEFAULT '',
  `cross_group_retry` tinyint(1) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_tokens_key` (`key`),
  KEY `idx_tokens_user_id` (`user_id`),
  KEY `idx_tokens_name` (`name`),
  KEY `idx_tokens_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `tokens`
--

LOCK TABLES `tokens` WRITE;
/*!40000 ALTER TABLE `tokens` DISABLE KEYS */;
/*!40000 ALTER TABLE `tokens` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `top_ups`
--

DROP TABLE IF EXISTS `top_ups`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `top_ups` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `amount` bigint DEFAULT NULL,
  `money` double DEFAULT NULL,
  `trade_no` varchar(255) DEFAULT NULL,
  `payment_method` varchar(50) DEFAULT NULL,
  `payment_provider` varchar(50) DEFAULT '',
  `create_time` bigint DEFAULT NULL,
  `complete_time` bigint DEFAULT NULL,
  `status` longtext,
  PRIMARY KEY (`id`),
  UNIQUE KEY `trade_no` (`trade_no`),
  KEY `idx_top_ups_user_id` (`user_id`),
  KEY `idx_top_ups_trade_no` (`trade_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `top_ups`
--

LOCK TABLES `top_ups` WRITE;
/*!40000 ALTER TABLE `top_ups` DISABLE KEYS */;
/*!40000 ALTER TABLE `top_ups` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `two_fa_backup_codes`
--

DROP TABLE IF EXISTS `two_fa_backup_codes`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `two_fa_backup_codes` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint NOT NULL,
  `code_hash` varchar(255) NOT NULL,
  `is_used` tinyint(1) DEFAULT NULL,
  `used_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_two_fa_backup_codes_user_id` (`user_id`),
  KEY `idx_two_fa_backup_codes_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `two_fa_backup_codes`
--

LOCK TABLES `two_fa_backup_codes` WRITE;
/*!40000 ALTER TABLE `two_fa_backup_codes` DISABLE KEYS */;
/*!40000 ALTER TABLE `two_fa_backup_codes` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `two_fas`
--

DROP TABLE IF EXISTS `two_fas`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `two_fas` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint NOT NULL,
  `secret` varchar(255) NOT NULL,
  `is_enabled` tinyint(1) DEFAULT NULL,
  `failed_attempts` bigint DEFAULT '0',
  `locked_until` datetime(3) DEFAULT NULL,
  `last_used_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `user_id` (`user_id`),
  KEY `idx_two_fas_user_id` (`user_id`),
  KEY `idx_two_fas_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `two_fas`
--

LOCK TABLES `two_fas` WRITE;
/*!40000 ALTER TABLE `two_fas` DISABLE KEYS */;
/*!40000 ALTER TABLE `two_fas` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `user_oauth_bindings`
--

DROP TABLE IF EXISTS `user_oauth_bindings`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `user_oauth_bindings` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint NOT NULL,
  `provider_id` bigint NOT NULL,
  `provider_user_id` varchar(256) NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_user_provider` (`user_id`,`provider_id`),
  UNIQUE KEY `ux_provider_userid` (`provider_id`,`provider_user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `user_oauth_bindings`
--

LOCK TABLES `user_oauth_bindings` WRITE;
/*!40000 ALTER TABLE `user_oauth_bindings` DISABLE KEYS */;
/*!40000 ALTER TABLE `user_oauth_bindings` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `user_subscriptions`
--

DROP TABLE IF EXISTS `user_subscriptions`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `user_subscriptions` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `user_id` bigint DEFAULT NULL,
  `plan_id` bigint DEFAULT NULL,
  `amount_total` bigint NOT NULL DEFAULT '0',
  `amount_used` bigint NOT NULL DEFAULT '0',
  `start_time` bigint DEFAULT NULL,
  `end_time` bigint DEFAULT NULL,
  `status` varchar(32) DEFAULT NULL,
  `source` varchar(32) DEFAULT 'order',
  `last_reset_time` bigint DEFAULT '0',
  `next_reset_time` bigint DEFAULT '0',
  `upgrade_group` varchar(64) DEFAULT '',
  `prev_user_group` varchar(64) DEFAULT '',
  `created_at` bigint DEFAULT NULL,
  `updated_at` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_user_subscriptions_user_id` (`user_id`),
  KEY `idx_user_sub_active` (`user_id`,`status`,`end_time`),
  KEY `idx_user_subscriptions_plan_id` (`plan_id`),
  KEY `idx_user_subscriptions_end_time` (`end_time`),
  KEY `idx_user_subscriptions_status` (`status`),
  KEY `idx_user_subscriptions_next_reset_time` (`next_reset_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `user_subscriptions`
--

LOCK TABLES `user_subscriptions` WRITE;
/*!40000 ALTER TABLE `user_subscriptions` DISABLE KEYS */;
/*!40000 ALTER TABLE `user_subscriptions` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `users`
--

DROP TABLE IF EXISTS `users`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `users` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `username` varchar(191) DEFAULT NULL,
  `password` longtext NOT NULL,
  `display_name` varchar(191) DEFAULT NULL,
  `role` bigint DEFAULT '1',
  `status` bigint DEFAULT '1',
  `email` varchar(191) DEFAULT NULL,
  `github_id` varchar(191) DEFAULT NULL,
  `discord_id` varchar(191) DEFAULT NULL,
  `oidc_id` varchar(191) DEFAULT NULL,
  `wechat_id` varchar(191) DEFAULT NULL,
  `telegram_id` varchar(191) DEFAULT NULL,
  `access_token` char(32) DEFAULT NULL,
  `quota` bigint DEFAULT '0',
  `used_quota` bigint DEFAULT '0',
  `request_count` bigint DEFAULT '0',
  `group` varchar(64) DEFAULT 'default',
  `aff_code` varchar(32) DEFAULT NULL,
  `aff_count` bigint DEFAULT '0',
  `aff_quota` bigint DEFAULT '0',
  `aff_history` bigint DEFAULT '0',
  `inviter_id` bigint DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  `linux_do_id` varchar(191) DEFAULT NULL,
  `setting` text,
  `remark` varchar(255) DEFAULT NULL,
  `stripe_customer` varchar(64) DEFAULT NULL,
  `created_at` bigint DEFAULT NULL,
  `last_login_at` bigint DEFAULT '0',
  PRIMARY KEY (`id`),
  UNIQUE KEY `username` (`username`),
  UNIQUE KEY `idx_users_access_token` (`access_token`),
  UNIQUE KEY `idx_users_aff_code` (`aff_code`),
  KEY `idx_users_stripe_customer` (`stripe_customer`),
  KEY `idx_users_username` (`username`),
  KEY `idx_users_email` (`email`),
  KEY `idx_users_git_hub_id` (`github_id`),
  KEY `idx_users_oidc_id` (`oidc_id`),
  KEY `idx_users_we_chat_id` (`wechat_id`),
  KEY `idx_users_display_name` (`display_name`),
  KEY `idx_users_discord_id` (`discord_id`),
  KEY `idx_users_telegram_id` (`telegram_id`),
  KEY `idx_users_inviter_id` (`inviter_id`),
  KEY `idx_users_deleted_at` (`deleted_at`),
  KEY `idx_users_linux_do_id` (`linux_do_id`)
) ENGINE=InnoDB AUTO_INCREMENT=2 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `users`
--

LOCK TABLES `users` WRITE;
/*!40000 ALTER TABLE `users` DISABLE KEYS */;
INSERT INTO `users` VALUES (1,'root','$2a$10$fwXTZTBPwxQrMSXoR69XgOH0d3JCVrSozKNMAxRz9rYmIA8TPmW46','Root User',100,1,'','','','','','','cDtGJIRjQxk8G5LjLRvkMVlmvcTz7Q==',100000000,0,0,'default','',0,0,0,0,NULL,'','','','',1781621585,1781621586);
/*!40000 ALTER TABLE `users` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `vendors`
--

DROP TABLE IF EXISTS `vendors`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `vendors` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `name` varchar(128) NOT NULL,
  `description` text,
  `icon` varchar(128) DEFAULT NULL,
  `status` bigint DEFAULT '1',
  `created_time` bigint DEFAULT NULL,
  `updated_time` bigint DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_vendor_name_delete_at` (`name`,`deleted_at`),
  KEY `idx_vendors_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `vendors`
--

LOCK TABLES `vendors` WRITE;
/*!40000 ALTER TABLE `vendors` DISABLE KEYS */;
/*!40000 ALTER TABLE `vendors` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

-- Dump completed on 2026-06-16 14:55:28
