variable "enabled" { type = bool }
variable "name" { type = string }
variable "account_id" { type = string }
variable "region" { type = string }
variable "ami_id" { type = string }
variable "instance_type" { type = string }
variable "architecture" { type = string }
variable "subnet_ids" { type = list(string) }
variable "security_group_id" { type = string }
variable "instance_profile" { type = string }
variable "service_linked_role_arn" { type = string }
variable "kms_key_arn" { type = string }
variable "target_group_arn" { type = string }
variable "data_bucket_name" { type = string }
variable "release_sha256" { type = string }
variable "release_version_id" { type = string }
variable "runtime_secret_arn" { type = string }
variable "owner_ssh_public_key" {
  type    = string
  default = null
  validation {
    condition     = var.owner_ssh_public_key == null || can(regex("^ssh-ed25519 [A-Za-z0-9+/=]+( [^\\r\\n]+)?$", var.owner_ssh_public_key))
    error_message = "Owner SSH uses one reviewed ed25519 public key, never a private key."
  }
}

variable "environment" {
  type    = string
  default = "dev"
}
variable "owner" {
  type    = string
  default = "Hung"
}
