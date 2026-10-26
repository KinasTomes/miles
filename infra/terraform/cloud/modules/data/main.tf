locals {
  # Physical attributes for KT §10.1; composite identity values are encoded by the adapter.
  tables = {
    Users             = { hash = "user_id", range = null, range_type = "S" }
    Buckets           = { hash = "bucket", range = null, range_type = "S" }
    BucketGrants      = { hash = "bucket_id", range = "user_id", range_type = "S" }
    AdminTokens       = { hash = "token_id", range = null, range_type = "S" }
    Credentials       = { hash = "access_key_id", range = null, range_type = "S" }
    SystemConfig      = { hash = "config_id", range = null, range_type = "S" }
    Objects           = { hash = "bucket", range = "key", range_type = "S" }
    ManifestPages     = { hash = "manifest_id", range = "page_no", range_type = "N" }
    Chunks            = { hash = "chunk_id", range = null, range_type = "S" }
    Packs             = { hash = "pack_id", range = "generation", range_type = "S" }
    Uploads           = { hash = "upload_id", range = null, range_type = "S" }
    UploadParts       = { hash = "upload_id", range = "part_number", range_type = "N" }
    UploadPartPages   = { hash = "part_manifest_id", range = "page_no", range_type = "N" }
    CompletionPages   = { hash = "completion_id", range = "page_no", range_type = "N" }
    Outbox            = { hash = "event_id", range = null, range_type = "S" }
    RecoveryEpochs    = { hash = "history_epoch_key", range = null, range_type = "S" }
    QuotaReservations = { hash = "reservation_id", range = null, range_type = "S" }
    Leases            = { hash = "resource_id", range = "lease_id", range_type = "S" }
    Jobs              = { hash = "job_id", range = null, range_type = "S" }
    Nodes             = { hash = "node_id", range = null, range_type = "S" }
  }
}
resource "aws_s3_bucket" "data" {
  bucket        = var.bucket_name
  force_destroy = false
  lifecycle { prevent_destroy = true }
}
resource "aws_s3_bucket_ownership_controls" "data" {
  bucket = aws_s3_bucket.data.id
  rule { object_ownership = "BucketOwnerEnforced" }
}
resource "aws_s3_bucket_public_access_block" "data" {
  bucket                  = aws_s3_bucket.data.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
resource "aws_s3_bucket_versioning" "data" {
  bucket = aws_s3_bucket.data.id
  versioning_configuration { status = "Enabled" }
}
resource "aws_s3_bucket_server_side_encryption_configuration" "data" {
  bucket = aws_s3_bucket.data.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }
    bucket_key_enabled = true
  }
}
resource "aws_s3_bucket_policy" "data" {
  bucket = aws_s3_bucket.data.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Sid = "TLSOnly", Effect = "Deny", Principal = "*", Action = "s3:*", Resource = [aws_s3_bucket.data.arn, "${aws_s3_bucket.data.arn}/*"], Condition = { Bool = { "aws:SecureTransport" = "false" } } },
      { Sid = "NoCustomerKey", Effect = "Deny", Principal = "*", Action = "s3:PutObject", Resource = "${aws_s3_bucket.data.arn}/*", Condition = { Null = { "s3:x-amz-server-side-encryption-customer-algorithm" = "false" } } },
      # Missing headers use bucket defaults; explicit overrides must use the same CMK.
      { Sid = "NoEncryptionOverride", Effect = "Deny", Principal = "*", Action = "s3:PutObject", Resource = "${aws_s3_bucket.data.arn}/*", Condition = { StringNotEquals = { "s3:x-amz-server-side-encryption" = "aws:kms" }, Null = { "s3:x-amz-server-side-encryption" = "false" } } },
      { Sid = "NoOtherKey", Effect = "Deny", Principal = "*", Action = "s3:PutObject", Resource = "${aws_s3_bucket.data.arn}/*", Condition = { StringNotEquals = { "s3:x-amz-server-side-encryption-aws-kms-key-id" = var.kms_key_arn }, Null = { "s3:x-amz-server-side-encryption-aws-kms-key-id" = "false" } } },
      { Sid = "ExplicitKMSNeedsKey", Effect = "Deny", Principal = "*", Action = "s3:PutObject", Resource = "${aws_s3_bucket.data.arn}/*", Condition = { StringEquals = { "s3:x-amz-server-side-encryption" = "aws:kms" }, Null = { "s3:x-amz-server-side-encryption-aws-kms-key-id" = "true" } } }
    ]
  })
}
resource "aws_dynamodb_table" "metadata" {
  for_each                    = local.tables
  name                        = "${var.name}-${each.key}"
  billing_mode                = "PAY_PER_REQUEST"
  hash_key                    = each.value.hash
  range_key                   = each.value.range
  deletion_protection_enabled = true
  attribute {
    name = each.value.hash
    type = "S"
  }
  dynamic "attribute" {
    for_each = each.value.range == null ? [] : [each.value.range]
    content {
      name = attribute.value
      type = each.value.range_type
    }
  }
  point_in_time_recovery {
    enabled                 = true
    recovery_period_in_days = 1
  }
  server_side_encryption {
    enabled     = true
    kms_key_arn = var.kms_key_arn
  }
  on_demand_throughput {
    max_read_request_units  = 10
    max_write_request_units = 10
  }
  lifecycle { prevent_destroy = true }
}
output "table_arns" { value = [for t in aws_dynamodb_table.metadata : t.arn] }
output "inventory" {
  value = {
    bucket_name = aws_s3_bucket.data.id
    tables      = { for n, t in aws_dynamodb_table.metadata : n => { name = t.name, hash_key = t.hash_key, range_key = t.range_key, range_type = local.tables[n].range_type } }
  }
}
