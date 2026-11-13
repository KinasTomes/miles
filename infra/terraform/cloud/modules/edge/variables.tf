variable "enabled" { type = bool }
variable "name" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "environment" { type = string }
variable "owner" { type = string }
variable "ami_id" { type = string }
variable "architecture" { type = string }
variable "instance_type" { type = string }
variable "subnet_id" { type = string }
variable "security_group_id" { type = string }
variable "instance_profile" { type = string }
variable "kms_key_arn" { type = string }
variable "data_bucket_name" { type = string }
variable "release_sha256" { type = string }
variable "release_version_id" { type = string }
variable "runtime_secret_arn" { type = string }
variable "bootstrap_secret_arn" { type = string }
variable "helper_sha256" { type = string }
variable "helper_version_id" { type = string }
variable "provider_config" {
  type = object({ trust_anchor_arn = string, profile_arn = string, role_arn = string })
}

variable "owner_ssh_public_key" { type = string }
