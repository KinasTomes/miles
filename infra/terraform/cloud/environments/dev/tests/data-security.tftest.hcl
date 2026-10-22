mock_provider "aws" {}
run "protected_data_contract" {
  command = plan
  module { source = "../../modules/data" }
  variables {
    name        = "miles-dev"
    bucket_name = "miles-dev-data-123456789012-ap-southeast-1"
    kms_key_arn = "arn:aws:kms:ap-southeast-1:123456789012:key/00000000-0000-0000-0000-000000000001"
  }
  assert {
    condition     = length(aws_dynamodb_table.metadata) == 20 && alltrue([for t in aws_dynamodb_table.metadata : t.billing_mode == "PAY_PER_REQUEST" && t.deletion_protection_enabled && t.point_in_time_recovery[0].enabled && t.point_in_time_recovery[0].recovery_period_in_days == 1 && t.server_side_encryption[0].kms_key_arn == var.kms_key_arn && length(t.ttl) == 0])
    error_message = "Metadata needs all 20 tables, CMK, 1-day PITR, deletion protection and no unreviewed TTL."
  }
  assert {
    condition     = aws_dynamodb_table.metadata["Objects"].hash_key == "bucket" && aws_dynamodb_table.metadata["Objects"].range_key == "key" && aws_dynamodb_table.metadata["Chunks"].hash_key == "chunk_id" && aws_dynamodb_table.metadata["Chunks"].range_key == null && aws_dynamodb_table.metadata["RecoveryEpochs"].hash_key == "history_epoch_key" && aws_dynamodb_table.metadata["ManifestPages"].range_key == "page_no" && one([for a in aws_dynamodb_table.metadata["Packs"].attribute : a.type if a.name == "generation"]) == "S"
    error_message = "Key mappings must match the documented physical adapter contract."
  }
  assert {
    condition     = !aws_s3_bucket.data.force_destroy && aws_s3_bucket_versioning.data.versioning_configuration[0].status == "Enabled" && aws_s3_bucket_ownership_controls.data.rule[0].object_ownership == "BucketOwnerEnforced" && aws_s3_bucket_public_access_block.data.block_public_acls && aws_s3_bucket_public_access_block.data.block_public_policy && aws_s3_bucket_public_access_block.data.ignore_public_acls && aws_s3_bucket_public_access_block.data.restrict_public_buckets && one(aws_s3_bucket_server_side_encryption_configuration.data.rule).apply_server_side_encryption_by_default[0].kms_master_key_id == var.kms_key_arn
    error_message = "Data bucket must retain versions, reject public ACLs/policies and use the runtime CMK."
  }
}
