@description('Name of the manual Container Apps Job.')
param jobName string

@description('Azure region of the existing Container Apps managed environment.')
param location string = resourceGroup().location

@description('Resource ID of the existing VNet-integrated Container Apps managed environment to canary.')
param managedEnvironmentId string

@minLength(64)
@maxLength(64)
@description('Lowercase sha256 digest of the exact compiler image to canary.')
param compilerImageDigest string

@description('Existing durable Azure Files environment storage name to canary.')
param replayStorageName string

@description('Exact workload profile to canary, including Consumption when applicable.')
param workloadProfileName string

module compilerJob './_aca-job.bicep' = {
  name: 'compiler-aca-canary'
  params: {
    jobName: jobName
    location: location
    managedEnvironmentId: managedEnvironmentId
    compilerImageDigest: compilerImageDigest
    replayStorageName: replayStorageName
    workloadProfileName: workloadProfileName
  }
}

output compilerJobId string = compilerJob.outputs.compilerJobId
