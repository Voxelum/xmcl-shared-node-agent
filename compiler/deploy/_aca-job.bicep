@description('Name of the manual Container Apps Job.')
param jobName string

@description('Azure region of the existing Container Apps managed environment.')
param location string

@description('Resource ID of an existing VNet-integrated Container Apps managed environment.')
param managedEnvironmentId string

@minLength(64)
@maxLength(64)
@description('Lowercase sha256 digest only. The repository and @sha256 prefix are fixed below.')
param compilerImageDigest string

@description('Existing Container Apps environment storage name for the durable replay/queue Azure Files share.')
param replayStorageName string

@description('Exact workload profile used by the job, including Consumption when applicable.')
param workloadProfileName string

var compilerImage = 'ghcr.io/voxelum/xmcl-shared-node-compiler@sha256:${compilerImageDigest}'

resource compilerJob 'Microsoft.App/jobs@2025-01-01' = {
  name: jobName
  location: location
  identity: {
    type: 'None'
  }
  properties: {
    environmentId: managedEnvironmentId
    workloadProfileName: workloadProfileName
    configuration: {
      triggerType: 'Manual'
      replicaTimeout: 1200
      replicaRetryLimit: 0
      manualTriggerConfig: {
        parallelism: 1
        replicaCompletionCount: 1
      }
      secrets: []
    }
    template: {
      containers: [
        {
          name: 'compiler'
          image: compilerImage
          args: [
            'probe'
          ]
          env: []
          resources: {
            cpu: json('2.0')
            memory: '4Gi'
          }
          volumeMounts: [
            {
              mountPath: '/run/xmcl-compiler'
              volumeName: 'runtime'
            }
            {
              mountPath: '/var/lib/xmcl-compiler'
              volumeName: 'state'
            }
          ]
        }
      ]
      volumes: [
        {
          name: 'runtime'
          storageType: 'EmptyDir'
        }
        {
          name: 'state'
          storageType: 'AzureFile'
          storageName: replayStorageName
          mountOptions: 'uid=10001,gid=10001,dir_mode=0700,file_mode=0600'
        }
      ]
    }
  }
}

output compilerJobId string = compilerJob.id
